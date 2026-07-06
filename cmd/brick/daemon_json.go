package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// daemonJSONOutput is the sole line of output brick prints for `-d --json`,
// meant to be parsed deterministically by the companion app that launches
// brick in daemon mode rather than by a human reading a terminal.
type daemonJSONOutput struct {
	Status  string `json:"status"` // "ok" | "error"
	PID     int    `json:"pid,omitempty"`
	LogPath string `json:"logPath,omitempty"`
	Folder  string `json:"folder,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// emitDaemonJSON prints out as the single line of JSON on stdout and
// terminates the process: 0 on success, 1 otherwise. Callers never return
// past this call.
func emitDaemonJSON(out daemonJSONOutput) {
	data, err := json.Marshal(out)
	if err != nil {
		// Should be unreachable (daemonJSONOutput only has marshalable fields),
		// but a companion app parsing stdout must never see anything other
		// than a single valid JSON line.
		data, _ = json.Marshal(daemonJSONOutput{Status: "error", Code: "internal_error", Message: err.Error()})
	}
	fmt.Println(string(data))
	if out.Status != "ok" {
		os.Exit(1)
	}
	os.Exit(0)
}
