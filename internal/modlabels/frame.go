// Package modlabels follows a labeler's label stream (com.atproto.label.subscribeLabels)
// and stores every label and label removal in ClickHouse (table mod_labels).
//
// Labels are not signature-checked: the stream is read directly from the labeler's
// own service endpoint over TLS.
package modlabels

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// Label is one label or removal (com.atproto.label.defs#label).
type Label struct {
	Ver *int64 `cbor:"ver,omitempty"`
	Src string `cbor:"src"`           // DID of the labeler
	URI string `cbor:"uri"`           // at:// URI of a record, or a DID for a whole account
	CID string `cbor:"cid,omitempty"` // optional: the record version the label applies to
	Val string `cbor:"val"`
	Neg bool   `cbor:"neg,omitempty"` // true: removes an earlier label with the same src, uri, and val
	Cts string `cbor:"cts"`           // creation time
	Exp string `cbor:"exp,omitempty"` // optional expiry
	Sig []byte `cbor:"sig,omitempty"`
}

// Frame is one decoded stream message. Each WebSocket message is a DAG-CBOR header
// ({op, t}) followed by a DAG-CBOR body.
type Frame struct {
	Kind   string // "#labels", "#info", "error", or an unknown message type
	Seq    int64  // #labels
	Labels []Label
	Info   string // #info: name and message
	Err    string // error frame: error and message
}

type header struct {
	Op int64  `cbor:"op"`
	T  string `cbor:"t"`
}

type labelsBody struct {
	Seq    int64   `cbor:"seq"`
	Labels []Label `cbor:"labels"`
}

type textBody struct {
	Name    string `cbor:"name"`
	Error   string `cbor:"error"`
	Message string `cbor:"message"`
}

// DecodeFrame decodes one stream message.
func DecodeFrame(msg []byte) (Frame, error) {
	dec := cbor.NewDecoder(bytes.NewReader(msg))
	var h header
	if err := dec.Decode(&h); err != nil {
		return Frame{}, fmt.Errorf("frame header: %w", err)
	}
	switch {
	case h.Op == -1:
		var b textBody
		_ = dec.Decode(&b)
		return Frame{Kind: "error", Err: strings.TrimSpace(b.Error + ": " + b.Message)}, nil
	case h.T == "#labels":
		var b labelsBody
		if err := dec.Decode(&b); err != nil {
			return Frame{}, fmt.Errorf("labels body: %w", err)
		}
		return Frame{Kind: h.T, Seq: b.Seq, Labels: b.Labels}, nil
	case h.T == "#info":
		var b textBody
		_ = dec.Decode(&b)
		return Frame{Kind: h.T, Info: strings.TrimSpace(b.Name + ": " + b.Message)}, nil
	default:
		return Frame{Kind: h.T}, nil
	}
}

// Row mirrors the mod_labels table.
type Row struct {
	Src        string     `ch:"src"`
	URI        string     `ch:"uri"`
	SubjectDID string     `ch:"subject_did"` // the account the label is about (record author, or the account itself)
	IsAccount  uint8      `ch:"is_account"`  // 1 when the label applies to a whole account
	CID        string     `ch:"cid"`
	Val        string     `ch:"val"`
	Neg        uint8      `ch:"neg"`
	Cts        time.Time  `ch:"cts"`
	Exp        *time.Time `ch:"exp"`
	Seq        uint64     `ch:"seq"`
	ReceivedAt time.Time  `ch:"received_at"`
}

// ToRow converts a label from the frame with sequence number seq.
func ToRow(l Label, seq int64, received time.Time) Row {
	r := Row{Src: l.Src, URI: l.URI, CID: l.CID, Val: l.Val, Seq: uint64(max(seq, 0)), ReceivedAt: received}
	if l.Neg {
		r.Neg = 1
	}
	switch {
	case strings.HasPrefix(l.URI, "did:"):
		r.SubjectDID, r.IsAccount = l.URI, 1
	case strings.HasPrefix(l.URI, "at://"):
		r.SubjectDID, _, _ = strings.Cut(strings.TrimPrefix(l.URI, "at://"), "/")
	}
	r.Cts = parseTime(l.Cts, received)
	if l.Exp != "" {
		t := parseTime(l.Exp, received)
		r.Exp = &t
	}
	return r
}

func parseTime(s string, fallback time.Time) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return fallback
}
