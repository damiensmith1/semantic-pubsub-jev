// Package web serves the console: a live view of what routing decided, an
// explorer for recorded measurement runs, and the project's findings.
//
// It observes rather than re-judges. Publishing goes through the real bus
// and the real judge; the judge records what it decided and this package
// reads that back. Judging a second time to populate a UI would double
// the cost and could disagree with the decision actually applied.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/damiensmith1/semantic-pubsub-jev/internal/interest"
	"github.com/damiensmith1/semantic-pubsub-jev/internal/judge"
)

//go:embed static
var assets embed.FS

// Publisher is the part of the bus this package needs.
type Publisher interface {
	PublishTopic(ctx context.Context, topic string, data json.RawMessage, reportTo string) (string, error)
}

// Options configures the handler.
type Options struct {
	Store     *interest.Store
	Recorder  *judge.Recorder
	Publisher Publisher
	Threshold float64

	// Topics are the topics the console offers. Empty means the console
	// still works; it just has nothing to suggest.
	Topics []string

	// ResultsDir holds the measurement harness's JSONL output, served to
	// the explorer. Missing is not an error — the explorer simply reports
	// that no runs have been recorded.
	ResultsDir string

	// JudgeLive is false when the server is running on the keyword judge,
	// so the console can say so rather than implying Jev answered.
	JudgeLive bool
	Model     string

	Log *slog.Logger
}

// Handler serves the console and its API.
func Handler(opts Options) (http.Handler, error) {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("web: embedded assets: %w", err)
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/config", opts.config)
	mux.HandleFunc("/api/subscribers", opts.subscribers)
	mux.HandleFunc("/api/publish", opts.publish)
	mux.HandleFunc("/api/decisions", opts.decisions)
	mux.HandleFunc("/api/stream", opts.stream)
	mux.HandleFunc("/api/runs", opts.runs)
	return mux, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// config reports the console's starting state. Topics are the configured
// suggestions merged with whatever is actually in use — there is no topic
// registry, so a topic exists because something subscribed to it.
func (o Options) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"topics":    o.mergedTopics(r),
		"threshold": o.Threshold,
		"judgeLive": o.JudgeLive,
		"model":     o.Model,
		"runs":      o.runNames(),
	})
}

// subscribers lists, adds and removes interests.
//
// A connection ID is required on write: interests are keyed by connection
// in the real system, and the console impersonating one keeps the same
// shape rather than inventing a parallel store the broker would ignore.
func (o Options) subscribers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		topic := r.URL.Query().Get("topic")
		if topic == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic is required"})
			return
		}
		cands, err := o.Store.Candidates(ctx, topic)
		if err != nil {
			o.Log.Error("list interests failed", "topic", topic, "err", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read interests"})
			return
		}
		out := make([]map[string]any, 0, len(cands))
		for _, c := range cands {
			out = append(out, map[string]any{"id": c.ID, "criteria": c.Criteria})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i]["id"].(string) < out[j]["id"].(string)
		})
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		var body struct{ Topic, ID, Criteria string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
			return
		}
		body.Criteria = strings.TrimSpace(body.Criteria)
		if body.Topic == "" || body.ID == "" || body.Criteria == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic, id and criteria are required"})
			return
		}
		if err := o.Store.Set(ctx, body.Topic, body.ID, body.Criteria); err != nil {
			o.Log.Error("set interest failed", "err", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not record interest"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": body.ID})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
			return
		}
		if err := o.Store.Drop(ctx, id); err != nil {
			o.Log.Error("drop interest failed", "err", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not remove interest"})
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// publish sends a message through the real bus. The routing decision
// arrives separately, via the recorder, because that is the decision that
// was actually applied.
func (o Options) publish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Topic == "" || len(body.Data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic and data are required"})
		return
	}
	// A topic with no interests never reaches the judge, so nothing would
	// be recorded and the console would show no trace of the publish.
	// Checking first costs one Redis round trip and keeps the feed honest.
	cands, cErr := o.Store.Candidates(r.Context(), body.Topic)
	if cErr != nil {
		o.Log.Warn("candidate lookup for publish record failed", "topic", body.Topic, "err", cErr.Error())
	}

	id, err := o.Publisher.PublishTopic(r.Context(), body.Topic, body.Data, "")
	if err != nil {
		o.Log.Error("publish failed", "topic", body.Topic, "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "publish failed"})
		return
	}
	if cErr == nil && len(cands) == 0 {
		o.Recorder.AddPublish(body.Topic, body.Data)
	}
	writeJSON(w, http.StatusOK, map[string]string{"streamId": id})
}

func (o Options) decisions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.Recorder.Recent(50))
}

// stream pushes judgments as they happen, over server-sent events.
func (o Options) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, release := o.Recorder.Subscribe()
	defer release()

	// A comment line opens the stream so the client's onopen fires even
	// before anything is judged.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case rec, open := <-ch:
			if !open {
				return
			}
			b, err := json.Marshal(rec)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// mergedTopics combines the configured suggestions with the topics that
// currently hold interests, so a topic someone typed in the console shows
// up for the next visitor without being registered anywhere.
func (o Options) mergedTopics(r *http.Request) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range o.Topics {
		add(t)
	}
	if live, err := o.Store.Topics(r.Context()); err == nil {
		for _, t := range live {
			add(t)
		}
	} else {
		o.Log.Warn("list topics failed", "err", err.Error())
	}
	sort.Strings(out)
	return out
}

func (o Options) runNames() []string {
	if o.ResultsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(o.ResultsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// runs serves one recorded measurement file to the explorer.
func (o Options) runs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusOK, o.runNames())
		return
	}
	// Reject anything that is not a plain file name in the results
	// directory: the name comes from a query string.
	if name != filepath.Base(name) || !strings.HasSuffix(name, ".jsonl") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad run name"})
		return
	}
	f, err := os.Open(filepath.Join(o.ResultsDir, name))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such run"})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	http.ServeContent(w, r, name, time.Time{}, f)
}
