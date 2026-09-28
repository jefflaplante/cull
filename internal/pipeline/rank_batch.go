package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/llm"
)

// batchExec runs rank calls as Message Batches (half price, asynchronous).
//
// Every batch it sends is recorded in statePath before it is polled, and every
// answer once collected, keyed by the frames its call sent (framesKey). A re-run
// first collects every recorded batch still processing (it is paid for), then
// reuses the recorded answer of any call that sends the same frames. Only the
// other calls are sent, and only their frames are decoded. The model never sees a
// call's ID, so an answer still applies when a set is renumbered.
type batchExec struct {
	client    BatchClient
	cfg       Config
	statePath string
}

// errBatchPending marks rank calls a batch executor couldn't answer yet (submit
// outcome unknown, polling or fetching results failed, Ctrl-C). What it sent stays
// recorded for a re-run. It stops the stage.
var errBatchPending = errors.New("batch ranking unfinished")

const rankBatchStateVersion = 1

func rankBatchStatePath(cfg Config) string { return cfg.ReportPath + ".rank-batch.json" }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// rankBatchState is a ranking's batches and the answers collected from them.
type rankBatchState struct {
	Version int                    `json:"version"`
	Backend string                 `json:"backend"`
	Model   string                 `json:"model"`
	Batches []*batchRecord         `json:"batches"`           // custom IDs from rankCustomID
	Answers map[string]*rankAnswer `json:"answers,omitempty"` // by framesKey, once collected
}

// rankAnswer is one collected answer, kept raw: it is decoded against the frames of
// the call it is handed to. Its usage reaches the report once:
//   - handed over to a set with its usage (Handed), then marked Charged once the
//     report holding that charge is saved (commit);
//   - an answer never handed over (its set changed) is charged when the ranking
//     settles.
type rankAnswer struct {
	Call    string          `json:"call"` // the custom ID it answered
	JSON    json.RawMessage `json:"json,omitempty"`
	Usage   llm.Usage       `json:"usage"`
	Err     string          `json:"error,omitempty"`
	Handed  bool            `json:"handed,omitempty"`
	Charged bool            `json:"charged,omitempty"`
}

// framesKey identifies what a call asks: its frames, in the order sent.
func framesKey(files []string) string {
	h := sha256.Sum256([]byte(strings.Join(files, "\x00")))
	return hex.EncodeToString(h[:8])
}

// rankCustomID is a call's ID in a batch: "S3-C1-<framesKey>" (≤64 of [A-Za-z0-9_-]).
func rankCustomID(c rankCall) string { return c.ID + "-" + framesKey(c.Files) }

// keyOfCustomID returns the framesKey a custom ID ends with.
func keyOfCustomID(id string) string { return id[strings.LastIndexByte(id, '-')+1:] }

func (batchExec) batch() bool { return true }

func (e batchExec) config() Config {
	cfg := e.cfg
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	return cfg
}

func (e batchExec) pending(err error) error {
	return fmt.Errorf("%w: %w (recorded in %s: rerun with --batch to re-attach)", errBatchPending, err, e.statePath)
}

// Run answers a round of calls:
//   - it collects every recorded batch still processing;
//   - it answers each call from a recorded answer for its frames;
//   - it decodes and sends the rest, then collects them.
//
// A call left without an answer answers with an errBatchPending error. The
// exceptions are a call whose set has an unreadable frame, and one in a chunk the
// API rejected outright: those fail only their set.
func (e batchExec) Run(ctx context.Context, calls []rankCall, load loader) []rankOut {
	cfg := e.config()
	out := make([]rankOut, len(calls))
	for i, c := range calls {
		out[i].ID = c.ID
	}
	failAll := func(err error) []rankOut {
		for i := range out {
			out[i] = rankOut{ID: out[i].ID, Err: err}
		}
		return out
	}
	st, err := e.open(cfg)
	if err != nil {
		return failAll(err)
	}
	save := func() error { return saveState(e.statePath, st) }

	var perr error // leaves calls pending
	if err := e.collect(ctx, cfg, st, save); err != nil {
		perr = e.pending(err) // not sending more while a recorded batch may hold these calls
	}
	var send []int
	for i, c := range calls {
		if st.Answers[framesKey(c.Files)] == nil {
			send = append(send, i)
		}
	}
	if perr == nil && len(send) > 0 {
		perr = e.send(ctx, cfg, st, calls, send, load, save, out)
	}

	changed := false
	for i, c := range calls {
		a := st.Answers[framesKey(c.Files)]
		if a == nil {
			if out[i].Err == nil {
				out[i].Err = perr
			}
			if out[i].Err == nil {
				out[i].Err = errors.New("no answer in its batch")
			}
			continue
		}
		if !a.Charged {
			out[i].U = a.Usage
			if !a.Handed {
				a.Handed, changed = true, true
			}
		}
		if a.Err != "" {
			out[i].Err = errors.New(a.Err)
			continue
		}
		out[i].R, out[i].Err = eval.DecodeRank(a.JSON, len(c.Files)) // a non-permutation fails its set: no retry in a batch
	}
	if changed {
		if err := save(); err != nil {
			return failAll(e.pending(err)) // handed over nothing: the re-run hands it over
		}
	}
	return out
}

// send decodes the frames of the calls at idx and submits them chunk by chunk,
// then collects them. A chunk the API rejects outright (llm.ErrRejected, a 4xx)
// fails its calls, whose sets fall back to scores; nothing of it stays pending. It
// returns the error that leaves the remaining calls pending.
func (e batchExec) send(ctx context.Context, cfg Config, st *rankBatchState, calls []rankCall, idx []int, load loader, save func() error, out []rankOut) error {
	sub := make([]rankCall, len(idx))
	for k, i := range idx {
		sub[k] = calls[i]
	}
	if err := load(ctx, sub); err != nil {
		return e.pending(err)
	}
	tokens := cfg.RankTokens
	if tokens <= 0 {
		tokens = defaultRankTokens
	}
	var reqs []llm.BatchRequest
	at := make(map[string]int, len(idx)) // custom ID -> index into calls
	for k, i := range idx {
		if sub[k].Frames == nil {
			out[i].Err = errUnreadable
			continue
		}
		id := rankCustomID(sub[k])
		at[id] = i
		reqs = append(reqs, llm.BatchRequest{CustomID: id, Req: eval.RankRequest(sub[k].Frames, tokens)})
	}
	round := 1 // for the log and the record
	if strings.HasSuffix(calls[idx[0]].ID, "-F") {
		round = 2
	}
	for _, chunk := range chunkRequests(cfg, reqs) {
		err := submitChunk(ctx, cfg, e.client, &st.Batches, chunk, "rank", round, save)
		switch {
		case err == nil:
		case errors.Is(err, llm.ErrRejected):
			for _, r := range chunk {
				out[at[r.CustomID]].Err = err
			}
		default:
			return e.pending(err)
		}
	}
	if err := e.collect(ctx, cfg, st, save); err != nil {
		return e.pending(err)
	}
	return nil
}

// collect polls every recorded batch still processing until it ends and records
// the answers of all its calls, whether or not a current call needs them: each is
// paid for. It saves after each batch; a batch whose results fail stays
// "submitted", to be fetched again.
func (e batchExec) collect(ctx context.Context, cfg Config, st *rankBatchState, save func() error) error {
	schemaFor := func(string) map[string]any { return eval.RankSchema() }
	for _, b := range st.Batches {
		if b.Status != "submitted" {
			continue
		}
		status, err := await(ctx, cfg, e.client, b.ID)
		if err != nil {
			return err
		}
		got := map[string]*rankAnswer{}
		err = e.client.BatchResults(ctx, status.ResultsURL, schemaFor, func(r llm.BatchResult) {
			a := &rankAnswer{Call: r.CustomID}
			if r.Response != nil {
				a.JSON, a.Usage = r.Response.JSON, r.Response.Usage
			}
			if r.Err != nil {
				a.Err = r.Err.Error()
			}
			got[keyOfCustomID(r.CustomID)] = a
		})
		if err != nil {
			return fmt.Errorf("batch %s results: %w", b.ID, err)
		}
		for k, a := range got {
			if _, ok := st.Answers[k]; !ok { // one already recorded may be handed over or charged
				st.Answers[k] = a
			}
		}
		b.Status = "collected"
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}

// open loads the state (or starts one). It refuses a state from another backend or
// model, and any submission whose outcome is unknown: that batch may exist, so
// sending more could pay twice.
func (e batchExec) open(cfg Config) (*rankBatchState, error) {
	st, err := loadRankBatchState(e.statePath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &rankBatchState{Version: rankBatchStateVersion, Backend: cfg.Backend, Model: cfg.Model, Answers: map[string]*rankAnswer{}}, nil
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errBatchPending, err)
	case st.Backend != cfg.Backend || st.Model != cfg.Model:
		return nil, fmt.Errorf("%w: %s belongs to %s/%s, not %s/%s: rerun with that backend and model to re-attach, or delete it to start over",
			errBatchPending, e.statePath, st.Backend, st.Model, cfg.Backend, cfg.Model)
	}
	for _, b := range st.Batches {
		if b.Status == "submitting" {
			return nil, fmt.Errorf("%w: a rank batch submission was interrupted before its ID was recorded, so it may have been created: "+
				"check the Batches page in the Claude Console, then delete %s to start over (not resubmitting, to avoid paying twice)", errBatchPending, e.statePath)
		}
	}
	if st.Answers == nil {
		st.Answers = map[string]*rankAnswer{}
	}
	return st, nil
}

// settle, once every set is recorded, collects any recorded batch still processing
// (it is paid for, so it is never dropped). It returns the usage of the answers no
// set was handed (their sets changed), which the ranking's cost still includes.
// Nothing is marked here: if the report isn't saved, the re-run settles again.
func (e batchExec) settle(ctx context.Context) (llm.Usage, error) {
	var u llm.Usage
	if !fileExists(e.statePath) {
		return u, nil
	}
	cfg := e.config()
	st, err := e.open(cfg)
	if err != nil {
		return u, err
	}
	if err := e.collect(ctx, cfg, st, func() error { return saveState(e.statePath, st) }); err != nil {
		return u, e.pending(err)
	}
	n := 0
	for _, a := range st.Answers {
		if !a.Handed && !a.Charged {
			u.Add(a.Usage)
			n++
		}
	}
	if n > 0 {
		fmt.Fprintf(cfg.Log, "%d rank answer(s) paid for in a batch went unused (their sets changed): %d input / %d output tokens, counted in the ranking's cost\n",
			n, u.InputTokens, u.OutputTokens)
	}
	return u, nil
}

// commit runs once the report holding the ranking so far is saved. When the
// ranking finished, the state goes. Otherwise every answer handed over is marked
// Charged, so a re-run reuses it at no cost.
func (e batchExec) commit(finished bool) error {
	if finished {
		if err := os.Remove(e.statePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	st, err := loadRankBatchState(e.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	changed := false
	for _, a := range st.Answers {
		if a.Handed && !a.Charged {
			a.Charged, changed = true, true
		}
	}
	if !changed {
		return nil
	}
	return saveState(e.statePath, st)
}

func loadRankBatchState(path string) (*rankBatchState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st rankBatchState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}
