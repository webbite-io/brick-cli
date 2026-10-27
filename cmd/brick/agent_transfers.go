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
	transferRunning   = "running"
	transferCompleted = "completed"
	transferFailed    = "failed"
	transferCanceled  = "canceled"
)

// transferRecord is one remote-control upload or download as reported by
// GET /transfers and GET /transfers/{id}. BytesDone can move backwards on an
// upload: authedStreamRequest resends the whole body after a token refresh.
type transferRecord struct {
	ID         string       `json:"id"`
	Direction  string       `json:"direction"` // "upload" or "download"
	State      string       `json:"state"`
	LocalPath  string       `json:"localPath"`
	NodeID     string       `json:"nodeId,omitempty"`   // download source
	ParentID   string       `json:"parentId,omitempty"` // upload destination folder
	Name       string       `json:"name"`
	BytesTotal int64        `json:"bytesTotal"`
	BytesDone  int64        `json:"bytesDone"`
	StartedAt  time.Time    `json:"startedAt"`
	UpdatedAt  time.Time    `json:"updatedAt"`
	FinishedAt *time.Time   `json:"finishedAt,omitempty"`
	Error      string       `json:"error,omitempty"`
	Node       *storageNode `json:"node,omitempty"` // uploaded node, once completed
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
		limit:  limit,
		byID:   map[string]*transferRecord{},
		active: map[string]*activeTransfer{},
	}
}

// begin registers rec as a running transfer and returns its ID. id is the
// caller-supplied transferId, if any: it must be a UUID not already in use,
// which lets the caller poll GET /transfers/{id} while a synchronous
// upload/download request is still in flight. An empty id gets a fresh one.
// cancel stops the context the transfer runs under; DELETE /transfers/{id}
// calls it.
func (tr *transferRegistry) begin(id string, rec transferRecord, cancel context.CancelFunc) (string, error) {
	if id == "" {
		id = uuid.NewString()
	} else if parsed, err := uuid.Parse(id); err != nil {
		return "", newAPIError(http.StatusBadRequest, "invalid_transfer_id", "transferId must be a UUID")
	} else {
		id = parsed.String()
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if _, exists := tr.byID[id]; exists {
		return "", newAPIError(http.StatusConflict, "transfer_exists", "a transfer with this transferId already exists")
	}
	now := time.Now().UTC()
	rec.ID = id
	rec.State = transferRunning
	rec.StartedAt = now
	rec.UpdatedAt = now
	tr.byID[id] = &rec
	tr.active[id] = &activeTransfer{rec: &rec, cancel: cancel, done: make(chan struct{})}
	return id, nil
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
	case ctx.Err() != nil:
		rec.State = transferCanceled
		rec.Error = err.Error()
	default:
		rec.State = transferFailed
		rec.Error = err.Error()
	}

	tr.history = append(tr.history, rec)
	for len(tr.history) > tr.limit {
		delete(tr.byID, tr.history[0].ID)
		tr.history = tr.history[1:]
	}
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
	sort.Slice(active, func(i, j int) bool { return active[i].StartedAt.Before(active[j].StartedAt) })
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
func (a *agentServer) handleTransfers(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state != "" && state != "active" && state != "history" {
		writeError(w, http.StatusBadRequest, "invalid_state", `state must be "active" or "history"`)
		return
	}
	active, history := a.transfers.list()
	resp := map[string]interface{}{"historyLimit": a.transfers.limit}
	if state != "history" {
		resp["active"] = active
	}
	if state != "active" {
		resp["history"] = history
	}
	writeJSON(w, http.StatusOK, resp)
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

// handleTransfer serves GET /transfers/{id}: one transfer's current status.
func (a *agentServer) handleTransfer(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.transfers.get(transferIDParam(r))
	if !ok {
		writeError(w, http.StatusNotFound, "transfer_not_found", "no transfer with this id in the current session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"transfer": rec})
}

// cancelTransferWait bounds how long DELETE /transfers/{id} waits for a
// cancelled transfer to stop before answering.
const cancelTransferWait = 5 * time.Second

// handleCancelTransfer serves DELETE /transfers/{id}: it cancels a running
// transfer and answers 200 with its record once it has stopped, or 202 if it
// is still winding down after cancelTransferWait (poll it to see it finish).
func (a *agentServer) handleCancelTransfer(w http.ResponseWriter, r *http.Request) {
	rec, stopped, err := a.transfers.cancel(transferIDParam(r), cancelTransferWait)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusOK
	if !stopped {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]interface{}{"transfer": rec})
}
