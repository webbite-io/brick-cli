package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/x/term"
	"github.com/google/uuid"
)

// --- CLI entry points ---

// transferOptions holds the flags shared by `brick upload` and
// `brick download`. overwrite only applies to upload.
type transferOptions struct {
	recursive bool
	silent    bool
	overwrite bool
}

// runUploadCmd handles `brick upload [-r] [-s] [--overwrite] <file|dir>
// [target-path|uuid]`.
func runUploadCmd(args []string) {
	fs := flag.NewFlagSet("upload", flag.ExitOnError)
	var opts transferOptions
	fs.BoolVar(&opts.recursive, "r", false, "")
	fs.BoolVar(&opts.recursive, "recursive", false, "")
	fs.BoolVar(&opts.silent, "s", false, "")
	fs.BoolVar(&opts.silent, "silent", false, "")
	fs.BoolVar(&opts.overwrite, "overwrite", false, "")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: brick upload [-r] [-s] [--overwrite] <file|dir> [target-path|uuid]")
	}
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	source := fs.Arg(0)
	target := ""
	if fs.NArg() >= 2 {
		target = fs.Arg(1)
	}

	sc, apiURL := mustTransferClient()
	ctx := context.Background()
	if err := runWithAutoRelogin(apiURL, authFailedReloginPrompt, func() error {
		return runUpload(ctx, sc, source, target, opts)
	}); err != nil {
		log.Fatalf("Upload failed: %v", err)
	}
}

// runDownloadCmd handles `brick download [-r] [-s] <uuid|path> [target-dir]`.
func runDownloadCmd(args []string) {
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	var opts transferOptions
	fs.BoolVar(&opts.recursive, "r", false, "")
	fs.BoolVar(&opts.recursive, "recursive", false, "")
	fs.BoolVar(&opts.silent, "s", false, "")
	fs.BoolVar(&opts.silent, "silent", false, "")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: brick download [-r] [-s] <uuid|path> [target-dir]")
	}
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	source := fs.Arg(0)
	target := ""
	if fs.NArg() >= 2 {
		target = fs.Arg(1)
	}

	sc, apiURL := mustTransferClient()
	ctx := context.Background()
	if err := runWithAutoRelogin(apiURL, authFailedReloginPrompt, func() error {
		return runDownload(ctx, sc, source, target, opts)
	}); err != nil {
		log.Fatalf("Download failed: %v", err)
	}
}

// mustTransferClient loads the local config and builds a storageClient for
// the active account, exiting with a clear message if brick isn't logged in
// or has no active account yet — the same precondition prepareSync enforces
// before a normal sync.
func mustTransferClient() (sc *storageClient, apiURL string) {
	apiURL = resolveAPIURL()
	storageURL := resolveStorageAPIURL()
	cfg, err := loadOrCreateConfig()
	if err != nil {
		log.Fatalf("%v", err)
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		log.Fatalf("brick is not logged in; run 'brick login' first")
	}
	if cfg.ActiveAccountID == "" {
		log.Fatalf("no active account selected; run 'brick switch-accounts' first")
	}
	return &storageClient{baseURL: storageURL, apiURL: apiURL, accountID: cfg.ActiveAccountID, cfg: cfg}, apiURL
}

// --- Argument resolution (UUID vs. remote path) ---

// resolveRemoteExisting resolves arg — a node UUID or a remote path like
// "/Documents/report.pdf" — to the node it already identifies. Used for a
// download's source and an upload's UUID target, both of which must already
// exist (unlike an upload's path target, which is created as needed — see
// resolveUploadTarget).
func resolveRemoteExisting(ctx context.Context, sc *storageClient, arg string) (*storageNode, error) {
	if id, err := uuid.Parse(arg); err == nil {
		return sc.getNode(ctx, id.String())
	}
	return sc.resolvePath(ctx, arg)
}

// resolveUploadTarget returns the folder ID an upload should land in: the
// account root if target is empty, the node itself if target is a UUID
// (validated as an existing folder), or the folder produced by creating (or
// reusing) each path segment "mkdir -p"-style if target is a path — reusing
// storageClient.createFolder's existing idempotent create-or-reuse behavior
// for each segment.
func resolveUploadTarget(ctx context.Context, sc *storageClient, target string) (string, error) {
	root, err := sc.resolveRoot(ctx)
	if err != nil {
		return "", err
	}
	if target == "" {
		return root.ID, nil
	}
	if id, err := uuid.Parse(target); err == nil {
		node, err := sc.getNode(ctx, id.String())
		if err != nil {
			return "", err
		}
		if node.NodeType != "folder" && node.NodeType != "root" {
			return "", fmt.Errorf("target %s is not a folder", target)
		}
		return node.ID, nil
	}
	cache := newRemoteFolderCache(ctx, sc, root.ID)
	return cache.ensure(strings.Trim(target, "/"))
}

// --- storageClient methods new to the transfer feature ---

func (sc *storageClient) getNode(ctx context.Context, id string) (*storageNode, error) {
	resp, err := sc.request(ctx, "GET", "/nodes/"+id, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, sc.errFrom(resp)
	}
	var node storageNode
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return nil, err
	}
	return &node, nil
}

// resolvePath resolves a human path string (e.g. "/Documents/report.pdf") to
// its node, matching brick-api's NodeService.ResolvePath: segments are split
// on "/", empty segments (leading/trailing/doubled slashes) are ignored, and
// an empty path resolves to the account root.
func (sc *storageClient) resolvePath(ctx context.Context, path string) (*storageNode, error) {
	resp, err := sc.request(ctx, "GET", "/resolve?path="+url.QueryEscape(path), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, sc.errFrom(resp)
	}
	var node storageNode
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return nil, err
	}
	return &node, nil
}

// downloadToFile streams GET /files/{id} directly to destPath, via a
// temporary file (the same tmpSuffix convention the sync engine's
// downloadFile uses) renamed into place atomically on success, rather than
// buffering the whole file in memory the way storageClient.download does —
// appropriate for a dedicated large-file transfer command. total is the
// file's known size (from the node metadata, fetched before this call — a
// chunked response may not carry a Content-Length), used only to report
// progress.
func (sc *storageClient) downloadToFile(ctx context.Context, nodeID, destPath string, total int64, onProgress func(written, total int64)) error {
	resp, err := sc.request(ctx, "GET", "/files/"+nodeID, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sc.errFrom(resp)
	}

	tmpPath := destPath + tmpSuffix
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	var written int64
	buf := make([]byte, 256*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				out.Close()
				os.Remove(tmpPath)
				return writeErr
			}
			written += int64(n)
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			out.Close()
			os.Remove(tmpPath)
			return readErr
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// authedStreamRequest mirrors authedRequest (see sync.go) but for a streaming
// request body — a large local file being uploaded — instead of one already
// held in memory as a []byte. body must support Seek so a 401/403 retry can
// rewind and resend it from the start, exactly like authedRequest's retry
// re-reads its []byte from the beginning; in practice body is always a local
// *os.File. size is set as Content-Length explicitly, since
// http.NewRequestWithContext only infers it for a handful of concrete body
// types that don't include an arbitrary io.Reader. onProgress, if set, is
// called with cumulative bytes sent as the transport reads the body; it
// starts over from zero on a retry, since a retry resends the whole body.
func authedStreamRequest(ctx context.Context, reqBaseURL, refreshAPIURL, method, path string, body io.ReadSeeker, size int64, headers map[string]string, cfg *Config, onProgress func(written int64)) (*http.Response, error) {
	client := &http.Client{Timeout: 30 * time.Minute}
	doRequest := func(token string) (*http.Response, error) {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		var r io.Reader = body
		if onProgress != nil {
			r = &progressReader{r: body, onRead: onProgress}
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(reqBaseURL, "/")+path, r)
		if err != nil {
			return nil, err
		}
		req.ContentLength = size
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return client.Do(req)
	}

	presented := currentAccessToken(cfg)
	resp, err := doRequest(presented)
	if err != nil {
		// See authedRequest: a pooled keep-alive connection the server closed
		// while idle surfaces as a transport error on the first attempt, not
		// an HTTP-level auth failure — retry once before giving up.
		resp, err = doRequest(presented)
		if err != nil {
			return nil, err
		}
	}
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && cfg.RefreshToken != "" {
		resp.Body.Close()
		newAccess, refreshErr := rotateAccessToken(refreshAPIURL, cfg, presented)
		if refreshErr != nil {
			return nil, fmt.Errorf("%w; token refresh failed: %v", errSessionExpired, refreshErr)
		}
		if newAccess == "" {
			return nil, fmt.Errorf("%w; no access token available (run 'brick login')", errSessionExpired)
		}
		return doRequest(newAccess)
	}
	return resp, nil
}

// progressReader wraps an io.Reader, reporting cumulative bytes read after
// every Read call.
type progressReader struct {
	r      io.Reader
	read   int64
	onRead func(read int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
		p.onRead(p.read)
	}
	return n, err
}

// streamRequest is the shared body of uploadStream/replaceStream: issue an
// authenticated streaming request and decode the resulting node.
func (sc *storageClient) streamRequest(ctx context.Context, method, path string, body io.ReadSeeker, size int64, headers map[string]string, onProgress func(written int64), wantStatus int) (*storageNode, error) {
	full := "/v1/accounts/" + sc.accountID + path
	resp, err := authedStreamRequest(ctx, sc.baseURL, sc.apiURL, method, full, body, size, headers, sc.cfg, onProgress)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		return nil, sc.errFrom(resp)
	}
	var result storageUploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result.Node, nil
}

// uploadStream is upload's streaming counterpart: POST /files without
// buffering the whole file in memory first, reporting progress as the body is
// read off disk and sent.
func (sc *storageClient) uploadStream(ctx context.Context, parentID, name string, r io.ReadSeeker, size int64, onProgress func(written int64)) (*storageNode, error) {
	headers := map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Parent-ID":  parentID,
		"X-Filename":   name,
	}
	return sc.streamRequest(ctx, "POST", "/files", r, size, headers, onProgress, http.StatusCreated)
}

// replaceStream is replace's streaming counterpart, used by `brick upload
// --overwrite` for a large file that already exists at the destination.
func (sc *storageClient) replaceStream(ctx context.Context, nodeID string, r io.ReadSeeker, size int64, onProgress func(written int64)) (*storageNode, error) {
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	return sc.streamRequest(ctx, "PUT", "/files/"+nodeID, r, size, headers, onProgress, http.StatusOK)
}

// walkRemoteTree walks the entire remote tree under rootID (breadth-first)
// and returns every file in it together with its path relative to rootID
// (slash-separated), plus the total folder count — the full-detail sibling of
// folderSummary, which only returns rootID's direct folder children and an
// aggregate size for the onboarding folder picker.
func (sc *storageClient) walkRemoteTree(ctx context.Context, rootID string) (files []remoteFileEntry, folderCount int, err error) {
	type queued struct {
		id  string
		rel string
	}
	queue := []queued{{id: rootID, rel: ""}}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		cur := queue[0]
		queue = queue[1:]
		children, err := sc.listChildren(ctx, cur.id)
		if err != nil {
			return nil, 0, err
		}
		for _, ch := range children {
			if ch.IsDeleted {
				continue
			}
			childRel := ch.Name
			if cur.rel != "" {
				childRel = cur.rel + "/" + ch.Name
			}
			switch ch.NodeType {
			case "folder":
				folderCount++
				queue = append(queue, queued{id: ch.ID, rel: childRel})
			case "file":
				files = append(files, remoteFileEntry{node: ch, relPath: childRel})
			}
		}
	}
	return files, folderCount, nil
}

type remoteFileEntry struct {
	node    storageNode
	relPath string // slash-separated, relative to the walk's root
}

// remoteFolderCache resolves (and creates, mkdir -p style) remote folders by
// their path relative to a base folder, and caches each folder's children
// listing — used by a recursive upload to avoid re-listing the same
// destination folder once per file when checking --overwrite conflicts.
type remoteFolderCache struct {
	sc       *storageClient
	ctx      context.Context
	baseID   string
	ids      map[string]string
	children map[string][]storageNode
}

func newRemoteFolderCache(ctx context.Context, sc *storageClient, baseID string) *remoteFolderCache {
	return &remoteFolderCache{
		sc:       sc,
		ctx:      ctx,
		baseID:   baseID,
		ids:      map[string]string{"": baseID},
		children: map[string][]storageNode{},
	}
}

// ensure returns the folder ID for relDir (slash-separated, relative to the
// cache's base folder), creating any missing segments along the way.
func (c *remoteFolderCache) ensure(relDir string) (string, error) {
	if id, ok := c.ids[relDir]; ok {
		return id, nil
	}
	parentRel, name := "", relDir
	if idx := strings.LastIndex(relDir, "/"); idx >= 0 {
		parentRel, name = relDir[:idx], relDir[idx+1:]
	}
	parentID, err := c.ensure(parentRel)
	if err != nil {
		return "", err
	}
	node, err := c.sc.createFolder(c.ctx, parentID, name)
	if err != nil {
		return "", err
	}
	c.ids[relDir] = node.ID
	return node.ID, nil
}

// childrenOf lists (and caches) a folder's children by ID, for --overwrite
// conflict lookups.
func (c *remoteFolderCache) childrenOf(folderID string) ([]storageNode, error) {
	if kids, ok := c.children[folderID]; ok {
		return kids, nil
	}
	kids, err := c.sc.listChildren(c.ctx, folderID)
	if err != nil {
		return nil, err
	}
	c.children[folderID] = kids
	return kids, nil
}

func findExistingFile(children []storageNode, name string) *storageNode {
	for i := range children {
		if children[i].NodeType == "file" && children[i].Name == name && !children[i].IsDeleted {
			return &children[i]
		}
	}
	return nil
}

// --- Local filesystem walk (upload side) ---

type localFileEntry struct {
	absPath string
	relPath string // slash-separated, relative to the upload source root
	size    int64
}

// walkLocalTree walks root and returns every regular file in it (relative
// paths always slash-separated, regardless of OS, so they line up with
// remote paths) plus the folder count and total size — the upload-side
// counterpart to walkRemoteTree, used both for the pre-flight stats banner
// and to drive the actual transfer.
func walkLocalTree(root string) (files []localFileEntry, folderCount int, totalSize int64, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			folderCount++
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		files = append(files, localFileEntry{absPath: path, relPath: rel, size: info.Size()})
		totalSize += info.Size()
		return nil
	})
	return files, folderCount, totalSize, err
}

// --- Progress UI ---

// progressPrinter renders per-file progress. On a TTY it redraws one line in
// place with \r; on a non-TTY stdout (piped/redirected) it prints exactly one
// line per file, on completion, so the output doesn't fill up with carriage
// returns. Entirely suppressed when silent.
type progressPrinter struct {
	silent bool
	isTTY  bool
	bar    progress.Model
}

func newProgressPrinter(silent bool) *progressPrinter {
	return &progressPrinter{
		silent: silent,
		isTTY:  term.IsTerminal(os.Stdout.Fd()),
		bar:    progress.New(progress.WithDefaultGradient(), progress.WithWidth(24)),
	}
}

// fileProgress reports the transfer of one file identified by label. done
// marks the final call for that file.
func (p *progressPrinter) fileProgress(label string, written, total int64, done bool) {
	if p.silent {
		return
	}
	displayTotal := total
	if displayTotal <= 0 {
		displayTotal = written
		if displayTotal == 0 {
			displayTotal = 1
		}
	}
	percent := float64(written) / float64(displayTotal)
	if percent > 1 {
		percent = 1
	}
	line := fmt.Sprintf("%s %s %s/%s", truncateLabel(label, 44), p.bar.ViewAs(percent), humanSize(written), humanSize(total))
	if p.isTTY {
		fmt.Printf("\r\033[K%s", line)
		if done {
			fmt.Println()
		}
		return
	}
	if done {
		fmt.Println(line)
	}
}

func truncateLabel(s string, max int) string {
	if len(s) <= max {
		return s + strings.Repeat(" ", max-len(s))
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// --- Result tracking & summary ---

// transferResult accumulates a recursive transfer's outcome: -r continues
// past a per-file error (rather than aborting the whole transfer), collects
// the failures, and reports them at the end.
type transferResult struct {
	succeeded int
	failed    []string
}

func (r *transferResult) fail(path string, err error) {
	r.failed = append(r.failed, fmt.Sprintf("%s: %v", path, err))
}

func (r *transferResult) ok() bool { return len(r.failed) == 0 }

// printTransferSummary prints the failure list (if any, always — even under
// -s, since -s only suppresses success-path output) and, unless silent, a
// final one-line summary.
func printTransferSummary(silent bool, verb string, r *transferResult, elapsed time.Duration) {
	if !r.ok() {
		fmt.Fprintln(os.Stderr, "\nFailed:")
		for _, f := range r.failed {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
	}
	if silent {
		return
	}
	fmt.Printf("%s %d file(s)", verb, r.succeeded)
	if len(r.failed) > 0 {
		fmt.Printf(", %d failed", len(r.failed))
	}
	fmt.Printf(" in %s\n", elapsed.Round(time.Second))
}

// --- Upload ---

func runUpload(ctx context.Context, sc *storageClient, source, target string, opts transferOptions) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("local source: %w", err)
	}

	parentID, err := resolveUploadTarget(ctx, sc, target)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}

	start := time.Now()
	pp := newProgressPrinter(opts.silent)

	if !info.IsDir() {
		result := &transferResult{}
		name := filepath.Base(source)
		if err := uploadSingleFile(ctx, sc, parentID, source, name, info.Size(), opts.overwrite, pp, "[1/1] "+name); err != nil {
			result.fail(name, err)
		} else {
			result.succeeded = 1
		}
		printTransferSummary(opts.silent, "Uploaded", result, time.Since(start))
		if !result.ok() {
			return errors.New("upload failed")
		}
		return nil
	}

	if !opts.recursive {
		return fmt.Errorf("%s is a folder; use -r to upload it recursively", source)
	}

	files, folderCount, totalSize, err := walkLocalTree(source)
	if err != nil {
		return fmt.Errorf("scanning %s: %w", source, err)
	}
	if !opts.silent {
		fmt.Printf("Uploading %d files in %d folders (%s)...\n", len(files), folderCount, humanSize(totalSize))
	}

	cache := newRemoteFolderCache(ctx, sc, parentID)
	result := &transferResult{}
	for i, f := range files {
		dir := ""
		if idx := strings.LastIndex(f.relPath, "/"); idx >= 0 {
			dir = f.relPath[:idx]
		}
		folderID, ferr := cache.ensure(dir)
		if ferr != nil {
			result.fail(f.relPath, ferr)
			continue
		}
		label := fmt.Sprintf("[%d/%d] %s", i+1, len(files), f.relPath)
		if err := uploadSingleFileCached(ctx, sc, cache, folderID, f.absPath, filepath.Base(f.absPath), f.size, opts.overwrite, pp, label); err != nil {
			result.fail(f.relPath, err)
			continue
		}
		result.succeeded++
	}
	printTransferSummary(opts.silent, "Uploaded", result, time.Since(start))
	if !result.ok() {
		return errors.New("some files failed to upload")
	}
	return nil
}

// uploadSingleFile handles a non-recursive `brick upload` of exactly one
// file, doing its own --overwrite lookup (there's no remoteFolderCache to
// reuse, since this is the whole transfer).
func uploadSingleFile(ctx context.Context, sc *storageClient, parentID, localPath, name string, size int64, overwrite bool, pp *progressPrinter, label string) error {
	existingID := ""
	if overwrite {
		kids, err := sc.listChildren(ctx, parentID)
		if err != nil {
			return err
		}
		if existing := findExistingFile(kids, name); existing != nil {
			existingID = existing.ID
		}
	}
	return doUploadFile(ctx, sc, parentID, existingID, localPath, name, size, pp, label)
}

// uploadSingleFileCached is uploadSingleFile's recursive-walk counterpart,
// using the shared remoteFolderCache's children cache instead of a fresh
// listChildren call per file.
func uploadSingleFileCached(ctx context.Context, sc *storageClient, cache *remoteFolderCache, folderID, localPath, name string, size int64, overwrite bool, pp *progressPrinter, label string) error {
	existingID := ""
	if overwrite {
		kids, err := cache.childrenOf(folderID)
		if err != nil {
			return err
		}
		if existing := findExistingFile(kids, name); existing != nil {
			existingID = existing.ID
		}
	}
	return doUploadFile(ctx, sc, folderID, existingID, localPath, name, size, pp, label)
}

// doUploadFile is the common tail of both upload paths above: open the local
// file, stream it to either uploadStream (new file) or replaceStream
// (existingID set, i.e. --overwrite found a conflict), and report progress.
func doUploadFile(ctx context.Context, sc *storageClient, parentID, existingID, localPath, name string, size int64, pp *progressPrinter, label string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	onProgress := func(written int64) { pp.fileProgress(label, written, size, false) }
	if existingID != "" {
		_, err = sc.replaceStream(ctx, existingID, f, size, onProgress)
	} else {
		_, err = sc.uploadStream(ctx, parentID, name, f, size, onProgress)
	}
	if err != nil {
		return err
	}
	pp.fileProgress(label, size, size, true)
	return nil
}

// --- Download ---

func runDownload(ctx context.Context, sc *storageClient, source, target string, opts transferOptions) error {
	node, err := resolveRemoteExisting(ctx, sc, source)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", source, err)
	}
	if target == "" {
		target = "."
	}

	start := time.Now()
	pp := newProgressPrinter(opts.silent)

	if node.NodeType == "file" {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		dest := filepath.Join(target, node.Name)
		result := &transferResult{}
		label := "[1/1] " + node.Name
		if err := sc.downloadToFile(ctx, node.ID, dest, node.SizeBytes, func(written, total int64) {
			pp.fileProgress(label, written, total, false)
		}); err != nil {
			result.fail(node.Name, err)
		} else {
			pp.fileProgress(label, node.SizeBytes, node.SizeBytes, true)
			result.succeeded = 1
		}
		printTransferSummary(opts.silent, "Downloaded", result, time.Since(start))
		if !result.ok() {
			return errors.New("download failed")
		}
		return nil
	}

	// folder (or root)
	if !opts.recursive {
		return fmt.Errorf("%s is a folder; use -r to download it recursively", source)
	}

	files, folderCount, err := sc.walkRemoteTree(ctx, node.ID)
	if err != nil {
		return fmt.Errorf("scanning %s: %w", source, err)
	}
	var totalSize int64
	for _, f := range files {
		totalSize += f.node.SizeBytes
	}
	if !opts.silent {
		fmt.Printf("Downloading %d files in %d folders (%s)...\n", len(files), folderCount, humanSize(totalSize))
	}

	result := &transferResult{}
	for i, f := range files {
		dest := filepath.Join(target, filepath.FromSlash(f.relPath))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			result.fail(f.relPath, err)
			continue
		}
		label := fmt.Sprintf("[%d/%d] %s", i+1, len(files), f.relPath)
		if err := sc.downloadToFile(ctx, f.node.ID, dest, f.node.SizeBytes, func(written, total int64) {
			pp.fileProgress(label, written, total, false)
		}); err != nil {
			result.fail(f.relPath, err)
			continue
		}
		pp.fileProgress(label, f.node.SizeBytes, f.node.SizeBytes, true)
		result.succeeded++
	}
	printTransferSummary(opts.silent, "Downloaded", result, time.Since(start))
	if !result.ok() {
		return errors.New("some files failed to download")
	}
	return nil
}
