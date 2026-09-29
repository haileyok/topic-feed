package modlabels

import (
	"bytes"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

func frame(t *testing.T, header, body any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := cbor.NewEncoder(&buf)
	if err := enc.Encode(header); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(body); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecodeLabelsFrame(t *testing.T) {
	msg := frame(t, map[string]any{"op": 1, "t": "#labels"}, map[string]any{
		"seq": 12345,
		"labels": []any{
			map[string]any{"ver": 1, "src": "did:plc:ar7c4by46qjdydhdevvrndac", "uri": "at://did:plc:author/app.bsky.feed.post/3abc",
				"cid": "bafypost", "val": "porn", "cts": "2026-09-29T04:30:00.123Z", "sig": []byte{1, 2, 3}},
			map[string]any{"src": "did:plc:ar7c4by46qjdydhdevvrndac", "uri": "did:plc:spammer", "val": "spam", "neg": true,
				"cts": "2026-09-29T04:30:01Z", "exp": "2026-10-29T00:00:00Z"},
		},
	})
	f, err := DecodeFrame(msg)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "#labels" || f.Seq != 12345 || len(f.Labels) != 2 {
		t.Fatalf("frame %+v", f)
	}
	now := time.Date(2026, 9, 29, 4, 30, 5, 0, time.UTC)

	r := ToRow(f.Labels[0], f.Seq, now)
	if r.Val != "porn" || r.SubjectDID != "did:plc:author" || r.IsAccount != 0 || r.Neg != 0 || r.CID != "bafypost" || r.Seq != 12345 {
		t.Errorf("post label row %+v", r)
	}
	if r.Cts.Format(time.RFC3339Nano) != "2026-09-29T04:30:00.123Z" || r.Exp != nil {
		t.Errorf("times %v %v", r.Cts, r.Exp)
	}

	r = ToRow(f.Labels[1], f.Seq, now)
	if r.Val != "spam" || r.SubjectDID != "did:plc:spammer" || r.IsAccount != 1 || r.Neg != 1 || r.Exp == nil {
		t.Errorf("account removal row %+v", r)
	}
}

func TestDecodeInfoAndErrorFrames(t *testing.T) {
	f, err := DecodeFrame(frame(t, map[string]any{"op": 1, "t": "#info"}, map[string]any{"name": "OutdatedCursor", "message": "too old"}))
	if err != nil || f.Kind != "#info" || f.Info != "OutdatedCursor: too old" {
		t.Errorf("info %+v %v", f, err)
	}
	f, err = DecodeFrame(frame(t, map[string]any{"op": -1}, map[string]any{"error": "FutureCursor", "message": "cursor in the future"}))
	if err != nil || f.Kind != "error" || f.Err != "FutureCursor: cursor in the future" {
		t.Errorf("error %+v %v", f, err)
	}
	if _, err := DecodeFrame([]byte{0xff, 0x00}); err == nil {
		t.Error("garbage should not decode")
	}
}

func TestStreamURL(t *testing.T) {
	s := &Stream{Cfg: Config{Service: "https://mod.bsky.app/"}}
	u, _ := s.streamURL(0, false)
	if u != "wss://mod.bsky.app/xrpc/com.atproto.label.subscribeLabels" {
		t.Errorf("live url %s", u)
	}
	u, _ = s.streamURL(987, true)
	if u != "wss://mod.bsky.app/xrpc/com.atproto.label.subscribeLabels?cursor=987" {
		t.Errorf("resume url %s", u)
	}
}
