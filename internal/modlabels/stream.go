package modlabels

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/coder/websocket"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/ingest"
)

// Config controls a stream consumer.
type Config struct {
	Service    string        // labeler service endpoint, e.g. https://mod.bsky.app
	Consumer   string        // row key in ingest_cursor
	FlushEvery time.Duration // write at least this often
	FlushRows  int           // or when this many rows are waiting
	IdleAfter  time.Duration // reconnect when no message arrives for this long
}

// Stream follows one labeler's label stream and writes mod_labels.
type Stream struct {
	Cfg  Config
	Conn driver.Conn
	Log  *slog.Logger
}

// Run follows the stream until ctx is cancelled, reconnecting with backoff. The first
// run starts live; later runs resume after the last written sequence number.
func (s *Stream) Run(ctx context.Context) error {
	cur := &ingest.Writer{Conn: s.Conn, Consumer: s.Cfg.Consumer}
	seq, haveSeq, err := cur.LoadCursor(ctx)
	if err != nil {
		return fmt.Errorf("load cursor: %w", err)
	}
	backoff := time.Second
	for {
		start := time.Now()
		next, err := s.runOnce(ctx, cur, seq, haveSeq)
		if next > 0 {
			seq, haveSeq = next, true
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fc futureCursor
		if errors.As(err, &fc) {
			s.Log.Warn("labeler says our cursor is in the future; restarting live", "cursor", seq)
			haveSeq = false
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		metricReconnects.Inc()
		s.Log.Warn("label stream ended; reconnecting", "err", err, "after", backoff, "cursor", seq)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

type futureCursor struct{ msg string }

func (e futureCursor) Error() string { return "future cursor: " + e.msg }

func (s *Stream) streamURL(seq uint64, haveSeq bool) (string, error) {
	u, err := url.Parse(strings.TrimRight(s.Cfg.Service, "/") + "/xrpc/com.atproto.label.subscribeLabels")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	if haveSeq {
		u.RawQuery = "cursor=" + strconv.FormatUint(seq, 10)
	}
	return u.String(), nil
}

// runOnce reads one connection until it fails. It returns the last sequence number
// whose labels were written (0 if none were written on this connection).
func (s *Stream) runOnce(ctx context.Context, cur *ingest.Writer, seq uint64, haveSeq bool) (uint64, error) {
	target, err := s.streamURL(seq, haveSeq)
	if err != nil {
		return 0, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, _, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{HTTPHeader: map[string][]string{"User-Agent": {"topic-feed-modlabels"}}})
	cancel()
	if err != nil {
		return 0, fmt.Errorf("dial %s: %w", target, err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(8 << 20)
	s.Log.Info("label stream connected", "url", target)

	type msg struct {
		data []byte
		err  error
	}
	msgs := make(chan msg, 256)
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	go func() {
		defer close(msgs)
		for {
			rctx, rcancel := context.WithTimeout(readCtx, s.Cfg.IdleAfter)
			_, data, err := ws.Read(rctx)
			rcancel()
			select {
			case msgs <- msg{data, err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var (
		pending []Row
		lastSeq uint64 // highest sequence number in pending
		written uint64 // highest sequence number written on this connection
	)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for attempt := 0; ; attempt++ {
			err := chdb.Insert(wctx, s.Conn, "mod_labels", pending)
			if err == nil {
				err = cur.SaveCursor(wctx, lastSeq)
			}
			if err == nil {
				break
			}
			metricFlushErrors.Inc()
			if attempt >= 4 || wctx.Err() != nil {
				return fmt.Errorf("write labels: %w", err)
			}
			s.Log.Warn("write failed; retrying", "err", err, "rows", len(pending))
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
		metricRowsWritten.Add(float64(len(pending)))
		metricSeq.Set(float64(lastSeq))
		written = lastSeq
		pending = pending[:0]
		return nil
	}
	defer func() { _ = flush() }()

	tick := time.NewTicker(s.Cfg.FlushEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		case <-tick.C:
			if err := flush(); err != nil {
				return written, err
			}
		case m, ok := <-msgs:
			if !ok {
				return written, errors.New("reader stopped")
			}
			if m.err != nil {
				if err := flush(); err != nil {
					return written, err
				}
				return written, fmt.Errorf("read: %w", m.err)
			}
			f, err := DecodeFrame(m.data)
			if err != nil {
				s.Log.Warn("undecodable frame; skipping", "err", err, "bytes", len(m.data))
				metricFrames.WithLabelValues("undecodable").Inc()
				continue
			}
			switch f.Kind {
			case "#labels":
				metricFrames.WithLabelValues("#labels").Inc()
				now := time.Now().UTC()
				for _, l := range f.Labels {
					r := ToRow(l, f.Seq, now)
					pending = append(pending, r)
					metricLabels.WithLabelValues(r.Val, strconv.Itoa(int(r.Neg))).Inc()
					metricLag.Set(now.Sub(r.Cts).Seconds())
				}
				if f.Seq > 0 {
					lastSeq = max(lastSeq, uint64(f.Seq))
				}
				if len(pending) >= s.Cfg.FlushRows {
					if err := flush(); err != nil {
						return written, err
					}
				}
			case "#info":
				metricFrames.WithLabelValues("#info").Inc()
				s.Log.Warn("labeler info", "info", f.Info)
			case "error":
				metricFrames.WithLabelValues("error").Inc()
				if strings.HasPrefix(f.Err, "FutureCursor") {
					return written, futureCursor{f.Err}
				}
				return written, fmt.Errorf("labeler error: %s", f.Err)
			default:
				metricFrames.WithLabelValues("other").Inc()
			}
		}
	}
}
