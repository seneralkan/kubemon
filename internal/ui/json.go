package ui

import (
	"encoding/json"
	"io"
	"time"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

// jsonOut writes NDJSON: one record per snapshot and per log line.
type jsonOut struct{ enc *json.Encoder }

type jsonRecord struct {
	Type     string          `json:"type"` // "snapshot" or "log"
	Snapshot *model.Snapshot `json:"snapshot,omitempty"`
	Log      *jsonLine       `json:"log,omitempty"`
}

// jsonLine mirrors logs.Line with lowerCamel keys.
type jsonLine struct {
	Time      time.Time `json:"time,omitzero"`
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Text      string    `json:"text"`
	Previous  bool      `json:"previous,omitempty"`
	Err       bool      `json:"err,omitempty"` // text is an error message, not log output
}

func newJSON(w io.Writer) *jsonOut { return &jsonOut{enc: json.NewEncoder(w)} }

func (j *jsonOut) snapshot(s model.Snapshot) {
	_ = j.enc.Encode(jsonRecord{Type: "snapshot", Snapshot: &s})
}

func (j *jsonOut) log(l logs.Line) {
	_ = j.enc.Encode(jsonRecord{Type: "log", Log: &jsonLine{
		Time: l.Time, Pod: l.Pod, Container: l.Container, Text: l.Text, Previous: l.Previous, Err: l.Err,
	}})
}
