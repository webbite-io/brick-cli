package main

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// defaultTransferHistoryLimit caps how many finished remote-control transfers
// the agent remembers for the current session when cfg.TransferHistoryLimit
// is unset.
const defaultTransferHistoryLimit = 1000

// Transfer states reported by the agent's /transfers endpoints.
const (
	transferQueued    = "queued" // waiting its turn in a batch upload
	transferRunning   = "running"
	transferCompleted = "completed"
	transferFailed    = "failed"
	transferCanceled  = "canceled"
)

// transferRecord is one remote-control upload or download as reported by
// GET /transfers and GET /transfers/{id}. BytesDone can move backwards on an
// upload: authedStreamRequest resends the whole body after a token refresh.
//
// A transfer queued by a batch upload holds the time it was queued in
// StartedAt until it starts running, and its ParentID is the batch's
// destination until the (sub)folder it goes into has been created.
type transferRecord struct {
	ID         string       `json:"id"`
	Direction  string       `json:"direction"` // "upload" or "download"
	State      string       `json:"state"`
	LocalPath  string       `json:"localPath"`
	NodeID     string       `json:"nodeId,omitempty"`   // download source
	ParentID   string       `json:"parentId,omitempty"` // upload destination folder
	Name       string       `json:"name"`
	BatchID    string       `json:"batchId,omitempty"`   // the batch upload it belongs to
	RemoteDir  string       `json:"remoteDir,omitempty"` // batch upload: folder path under the batch's parentId
	BytesTotal int64        `json:"bytesTotal"`
	BytesDone  int64        `json:"bytesDone"`
	StartedAt  time.Time    `json:"startedAt"`
	UpdatedAt  time.Time    `json:"updatedAt"`
	FinishedAt *time.Time   `json:"finishedAt,omitempty"`
	Error      string       `json:"error,omitempty"`
	Node       *storageNode `json:"node,omitempty"` // uploaded node, once completed

	seq uint64 // registration order, which the active list is sorted by
}

// maxBatchHistory caps how many finished batch uploads the agent remembers.
const maxBatchHistory = 100

// transferBatch is a batch upload as reported by GET /transfers/{id} for the
// batch's id: one transfer per file, run one at a time.
type transferBatch struct {
	ID          string     `json:"id"`
	State       string     `json:"state"` // queued, running, then completed, failed (any file failed) or canceled
	ParentID    string     `json:"parentId"`
	FileCount   int        `json:"fileCount"`
	TotalBytes  int64      `json:"totalBytes"`
	BytesDone   int64      `json:"bytesDone"` // completed files plus the running one's progress
	Completed   int        `json:"completed"`
	Failed      int        `json:"failed"`
	Canceled    int        `json:"canceled"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	TransferIDs []string   `json:"transferIds"`

	completedBytes int64
}

// transferRegistry tracks the remote-control transfers of the running
// session: every active one, plus the most recent finished ones up to limit.
// It lives only in memory, so a restart starts with an empty history.
type transferRegistry struct {
	mu      sync.Mutex
	limit   int
	byID    map[string]*transferRecord // active and history
	active  map[string]*activeTransfer
	history []*transferRecord // oldest first
	seq     uint64

	batches         map[string]*transferBatch // running and recently finished
	finishedBatches []string                  // oldest first, capped at maxBatchHistory
}

// activeTransfer is a running transfer's record plus what cancel needs to
// stop it: the cancel func of the context it runs under, and done, closed by
// finish once it has stopped.
type activeTransfer struct {
	rec    *transferRecord
	cancel context.CancelFunc
	done   chan struct{}
}

func newTransferRegistry(limit int) *transferRegistry {
	if limit <= 0 {
		limit = defaultTransferHistoryLimit
	}
	return &transferRegistry{
		limit:   limit,
		byID:    map[string]*transferRecord{},
		active:  map[string]*activeTransfer{},
		batches: map[string]*transferBatch{},
	}
}

// canonicalTransferID validates a caller-supplied transferId or batchId (a
// UUID, named field in the error) and returns it in canonical form; an empty
// id gets a fresh one.
func canonicalTransferID(id, code, field string) (string, error) {
	if id == "" {
		return uuid.NewString(), nil
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", newAPIError(http.StatusBadRequest, code, field+" must be a UUID")
	}
	return parsed.String(), nil
}

// begin registers rec as a running transfer and returns its ID. id is the
// caller-supplied transferId, if any: it must be a UUID not already in use,
// which lets the caller poll GET /transfers/{id} while a synchronous
// upload/download request is still in flight. An empty id gets a fresh one.
// cancel stops the context the transfer runs under; DELETE /transfers/{id}
// calls it.
func (tr *transferRegistry) begin(id string, rec transferRecord, cancel context.CancelFunc) (string, error) {
	id, err := canonicalTransferID(id, "invalid_transfer_id", "transferId")
	if err != nil {
		return "", err
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.idTakenLocked(id) {
		return "", newAPIError(http.StatusConflict, "transfer_exists", "a transfer with this transferId already exists")
	}
	tr.addLocked(id, rec, transferRunning, cancel)
	return id, nil
}

// idTakenLocked reports whether id names a known transfer or batch: the two
// share GET/DELETE /transfers/{id}, so their ids must not collide.
func (tr *transferRegistry) idTakenLocked(id string) bool {
	_, transfer := tr.byID[id]
	_, batch := tr.batches[id]
	return transfer || batch
}

func (tr *transferRegistry) addLocked(id string, rec transferRecord, state string, cancel context.CancelFunc) *transferRecord {
	now := time.Now().UTC()
	tr.seq++
	rec.ID = id
	rec.State = state
	rec.StartedAt = now
	rec.UpdatedAt = now
	rec.seq = tr.seq
	tr.byID[id] = &rec
	tr.active[id] = &activeTransfer{rec: &rec, cancel: cancel, done: make(chan struct{})}
	return &rec
}

// queuedTransfer is one file of a batch upload, as handed to beginBatch.
type queuedTransfer struct {
	rec    transferRecord
	cancel context.CancelFunc
}

// beginBatch registers batch upload batchID and one queued transfer per file,
// returning the batch's ids. As with begin, an empty batchID gets a fresh one.
func (tr *transferRegistry) beginBatch(batchID, parentID string, items []queuedTransfer) (transferBatch, error) {
	batchID, err := canonicalTransferID(batchID, "invalid_batch_id", "batchId")
	if err != nil {
		return transferBatch{}, err
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.idTakenLocked(batchID) {
		return transferBatch{}, newAPIError(http.StatusConflict, "batch_exists", "a transfer or batch with this batchId already exists")
	}
	now := time.Now().UTC()
	b := &transferBatch{
		ID: batchID, State: transferQueued, ParentID: parentID, FileCount: len(items),
		CreatedAt: now, UpdatedAt: now, TransferIDs: make([]string, 0, len(items)),
	}
	for _, it := range items {
		id := uuid.NewString()
		it.rec.BatchID = batchID
		rec := tr.addLocked(id, it.rec, transferQueued, it.cancel)
		b.TransferIDs = append(b.TransferIDs, id)
		b.TotalBytes += rec.BytesTotal
	}
	tr.batches[batchID] = b
	return tr.batchViewLocked(b), nil
}

// start moves queued transfer id to running. It is false if the transfer is
// no longer queued, e.g. because it was cancelled while it waited.
func (tr *transferRegistry) start(id string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	at, ok := tr.active[id]
	if !ok || at.rec.State != transferQueued {
		return false
	}
	now := time.Now().UTC()
	at.rec.State = transferRunning
	at.rec.StartedAt = now
	at.rec.UpdatedAt = now
	if b := tr.batches[at.rec.BatchID]; b != nil && b.State == transferQueued {
		b.State = transferRunning
		b.UpdatedAt = now
	}
	return true
}

// update applies fn to active transfer id, keeping its batch's TotalBytes in
// step with any change to BytesTotal (a file that changed size since it was
// queued).
func (tr *transferRegistry) update(id string, fn func(rec *transferRecord)) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	at, ok := tr.active[id]
	if !ok {
		return
	}
	before := at.rec.BytesTotal
	fn(at.rec)
	at.rec.UpdatedAt = time.Now().UTC()
	if b := tr.batches[at.rec.BatchID]; b != nil {
		b.TotalBytes += at.rec.BytesTotal - before
	}
}

// progress records how many bytes of transfer id have been moved so far.
func (tr *transferRegistry) progress(id string, done int64) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if at, ok := tr.active[id]; ok {
		at.rec.BytesDone = done
		at.rec.UpdatedAt = time.Now().UTC()
	}
}

// finish moves transfer id into the history, evicting the oldest entries
// beyond limit. ctx is the context the transfer ran under: an error after it
// was cancelled (DELETE /transfers/{id}, or brick shutting down) is reported
// as canceled, not failed.
func (tr *transferRegistry) finish(ctx context.Context, id string, node *storageNode, err error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.finishLocked(id, node, err, ctx.Err() != nil)
}

// finishLocked is finish with the cancellation already decided: canceled
// reports a failure as canceled rather than failed.
func (tr *transferRegistry) finishLocked(id string, node *storageNode, err error, canceled bool) {
	at, ok := tr.active[id]
	if !ok {
		return
	}
	delete(tr.active, id)
	// Deferred so the state below is decided by ctx as the transfer left it:
	// cancelling first would make every failure look like a cancellation.
	defer close(at.done)
	defer at.cancel() // release the context's resources
	rec := at.rec

	now := time.Now().UTC()
	rec.UpdatedAt = now
	rec.FinishedAt = &now
	switch {
	case err == nil:
		rec.State = transferCompleted
		rec.BytesDone = rec.BytesTotal
		rec.Node = node
	case canceled:
		rec.State = transferCanceled
		rec.Error = err.Error()
	default:
		rec.State = transferFailed
		rec.Error = err.Error()
	}
	if b := tr.batches[rec.BatchID]; b != nil {
		tr.recordBatchResultLocked(b, rec, now)
	}

	tr.history = append(tr.history, rec)
	for len(tr.history) > tr.limit {
		delete(tr.byID, tr.history[0].ID)
		tr.history = tr.history[1:]
	}
}

// recordBatchResultLocked counts finished transfer rec towards batch b, and
// finishes b once every file has.
func (tr *transferRegistry) recordBatchResultLocked(b *transferBatch, rec *transferRecord, now time.Time) {
	b.UpdatedAt = now
	switch rec.State {
	case transferCompleted:
		b.Completed++
		b.completedBytes += rec.BytesTotal
	case transferCanceled:
		b.Canceled++
	default:
		b.Failed++
	}
	if b.Completed+b.Failed+b.Canceled < b.FileCount {
		return
	}
	b.FinishedAt = &now
	switch {
	case b.Failed > 0:
		b.State = transferFailed
	case b.Canceled > 0:
		b.State = transferCanceled
	default:
		b.State = transferCompleted
	}
	tr.finishedBatches = append(tr.finishedBatches, b.ID)
	for len(tr.finishedBatches) > maxBatchHistory {
		delete(tr.batches, tr.finishedBatches[0])
		tr.finishedBatches = tr.finishedBatches[1:]
	}
}

// batchViewLocked returns a copy of b with BytesDone filled in.
func (tr *transferRegistry) batchViewLocked(b *transferBatch) transferBatch {
	view := *b
	view.TransferIDs = append([]string(nil), b.TransferIDs...)
	view.BytesDone = b.completedBytes
	if b.FinishedAt == nil {
		for _, id := range b.TransferIDs {
			if at, ok := tr.active[id]; ok && at.rec.State == transferRunning {
				view.BytesDone += at.rec.BytesDone
			}
		}
	}
	return view
}

// getBatch returns a copy of batch id.
func (tr *transferRegistry) getBatch(id string) (transferBatch, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	b, ok := tr.batches[id]
	if !ok {
		return transferBatch{}, false
	}
	return tr.batchViewLocked(b), true
}

// cancelBatch cancels every unfinished transfer of batch id: queued ones at
// once, the running one (if any) as cancel does, waiting up to wait for it.
func (tr *transferRegistry) cancelBatch(id string, wait time.Duration) (batch transferBatch, stopped bool, err error) {
	tr.mu.Lock()
	b, ok := tr.batches[id]
	if !ok {
		tr.mu.Unlock()
		return transferBatch{}, false, newAPIError(http.StatusNotFound, "transfer_not_found", "no transfer or batch with this id in the current session")
	}
	if b.FinishedAt != nil {
		tr.mu.Unlock()
		return transferBatch{}, false, newAPIError(http.StatusConflict, "transfer_finished", "the batch has already finished")
	}
	var running []*activeTransfer
	for _, tid := range b.TransferIDs {
		at, ok := tr.active[tid]
		if !ok {
			continue
		}
		if at.rec.State == transferQueued {
			tr.finishLocked(tid, nil, context.Canceled, true)
		} else {
			running = append(running, at)
		}
	}
	tr.mu.Unlock()

	stopped = true
	deadline := time.After(wait)
	for _, at := range running {
		at.cancel()
		select {
		case <-at.done:
		case <-deadline:
			stopped = false
		}
	}
	batch, _ = tr.getBatch(id)
	return batch, stopped, nil
}

// canceled reports whether transfer id finished as canceled.
func (tr *transferRegistry) canceled(id string) bool {
	rec, ok := tr.get(id)
	return ok && rec.State == transferCanceled
}

// get returns a copy of transfer id, active or finished.
func (tr *transferRegistry) get(id string) (transferRecord, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	rec, ok := tr.byID[id]
	if !ok {
		return transferRecord{}, false
	}
	return *rec, true
}

// list returns copies of the active transfers (oldest first) and of the
// history (newest first).
func (tr *transferRegistry) list() (active, history []transferRecord) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	active = make([]transferRecord, 0, len(tr.active))
	for _, at := range tr.active {
		active = append(active, *at.rec)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].seq < active[j].seq })
	history = make([]transferRecord, 0, len(tr.history))
	for i := len(tr.history) - 1; i >= 0; i-- {
		history = append(history, *tr.history[i])
	}
	return active, history
}

// cancel stops running transfer id and waits up to wait for it to finish.
// stopped is false if it was still winding down when wait ran out.
func (tr *transferRegistry) cancel(id string, wait time.Duration) (rec transferRecord, stopped bool, err error) {
	tr.mu.Lock()
	at, active := tr.active[id]
	_, known := tr.byID[id]
	if active && at.rec.State == transferQueued {
		// Never started, so there is nothing to wait for.
		tr.finishLocked(id, nil, context.Canceled, true)
		rec = *at.rec
		tr.mu.Unlock()
		return rec, true, nil
	}
	tr.mu.Unlock()
	if !active {
		if known {
			return transferRecord{}, false, newAPIError(http.StatusConflict, "transfer_finished", "the transfer has already finished")
		}
		return transferRecord{}, false, newAPIError(http.StatusNotFound, "transfer_not_found", "no transfer with this id in the current session")
	}

	at.cancel()
	select {
	case <-at.done:
		stopped = true
	case <-time.After(wait):
	}
	rec, _ = tr.get(id)
	return rec, stopped, nil
}

// handleTransfers serves GET /transfers: the session's active transfers and
// finished-transfer history. ?state=active or ?state=history returns only
// that list, so a frequent poller can skip the (up to limit-long) history.
// ?batchId= narrows both lists to one batch upload's files and adds the
// batch's summary as "batch".
func (a *agentServer) handleTransfers(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state != "" && state != "active" && state != "history" {
		writeError(w, http.StatusBadRequest, "invalid_state", `state must be "active" or "history"`)
		return
	}
	active, history := a.transfers.list()
	resp := map[string]interface{}{"historyLimit": a.transfers.limit}
	if batchID := r.URL.Query().Get("batchId"); batchID != "" {
		if parsed, err := uuid.Parse(batchID); err == nil {
			batchID = parsed.String()
		}
		batch, ok := a.transfers.getBatch(batchID)
		if !ok {
			writeError(w, http.StatusNotFound, "batch_not_found", "no batch with this id in the current session")
			return
		}
		resp["batch"] = batch
		active, history = filterBatch(active, batchID), filterBatch(history, batchID)
	}
	if state != "history" {
		resp["active"] = active
	}
	if state != "active" {
		resp["history"] = history
	}
	writeJSON(w, http.StatusOK, resp)
}

func filterBatch(recs []transferRecord, batchID string) []transferRecord {
	out := make([]transferRecord, 0, len(recs))
	for _, rec := range recs {
		if rec.BatchID == batchID {
			out = append(out, rec)
		}
	}
	return out
}

// transferIDParam returns the request's {id} path value in the canonical
// form begin stores.
func transferIDParam(r *http.Request) string {
	id := r.PathValue("id")
	if parsed, err := uuid.Parse(id); err == nil {
		id = parsed.String()
	}
	return id
}

// handleTransfer serves GET /transfers/{id}: one transfer's current status
// as {transfer}, or, for a batch upload's id, the batch's as {batch}.
func (a *agentServer) handleTransfer(w http.ResponseWriter, r *http.Request) {
	id := transferIDParam(r)
	if rec, ok := a.transfers.get(id); ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"transfer": rec})
		return
	}
	if batch, ok := a.transfers.getBatch(id); ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"batch": batch})
		return
	}
	writeError(w, http.StatusNotFound, "transfer_not_found", "no transfer with this id in the current session")
}

// cancelTransferWait bounds how long DELETE /transfers/{id} waits for a
// cancelled transfer to stop before answering.
const cancelTransferWait = 5 * time.Second

// handleCancelTransfer serves DELETE /transfers/{id}: it cancels a running
// or queued transfer and answers 200 with its record once it has stopped, or
// 202 if it is still winding down after cancelTransferWait (poll it to see it
// finish). For a batch upload's id it cancels every file not yet done and
// answers likewise with the batch as {batch}.
func (a *agentServer) handleCancelTransfer(w http.ResponseWriter, r *http.Request) {
	id := transferIDParam(r)
	status := http.StatusOK
	if _, isBatch := a.transfers.getBatch(id); isBatch {
		batch, stopped, err := a.transfers.cancelBatch(id, cancelTransferWait)
		if err != nil {
			writeErr(w, err)
			return
		}
		if !stopped {
			status = http.StatusAccepted
		}
		writeJSON(w, status, map[string]interface{}{"batch": batch})
		return
	}
	rec, stopped, err := a.transfers.cancel(id, cancelTransferWait)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !stopped {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]interface{}{"transfer": rec})
}
