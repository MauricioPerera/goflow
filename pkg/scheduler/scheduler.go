// Package scheduler is the "external scheduler" pkg/piece's own Trigger doc
// comment says a real polling trigger needs — the piece it drives
// (pkg/pieces/schedule) only decides WHETHER it's due when asked; nothing in
// this project called Run() on a timer before this package. Without it, a
// flow using schedule.PieceName/schedule.TriggerName is inert: saved, valid,
// runnable by hand via POST /flows/{name}/run, but never fired on its own.
//
// Scheduler ticks on its own interval (Tick — how often IT checks, not any
// one flow's configured intervalSeconds), lists every saved flow each tick,
// finds the ones whose trigger is the schedule piece, and asks each one's
// trigger whether it's due. A due flow runs through
// flowstore.RunWithHistory — the exact same path POST /flows/{name}/run and
// MCP's tools/call already share — so a schedule-fired run resolves/redacts
// credentials and lands in run history identically to a manually triggered
// one.
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces/schedule"
	"goflow/pkg/runstore"
)

// Scheduler owns one cursor Store shared across every schedule-triggered flow
// this process discovers, namespaced per flow via piece.ScopedStore — same
// "multiple flows, one trigger mechanism, don't let their cursors clash"
// reasoning ScopedStore's own doc comment already gives, applied here across
// different FLOWS sharing the schedule piece rather than across flows
// sharing one externally-polled trigger. In-memory only, deliberately: a
// restart loses every flow's cursor, so each schedule-triggered flow fires
// once more, immediately, on the first tick after a restart — the same
// "no cursor yet" behavior schedule.Run already has for a brand-new flow,
// not a new failure mode this package introduces. See pkg/runstore's doc
// comment for the same v1-default-not-a-durability-promise posture applied
// to a different store in this project.
type Scheduler struct {
	FlowStore     *flowstore.GatedStore
	BuildRegistry func() (*piece.Registry, error)
	CredStore     credentials.Store
	HistoryStore  runstore.Store
	Tick          time.Duration

	store *piece.MemoryStore

	mu      sync.Mutex
	running map[string]bool // flow name -> a run this scheduler started for it hasn't finished yet
}

// New returns a Scheduler ready to Run. tick is how often every saved flow
// is checked — independent of any individual flow's own intervalSeconds,
// which only controls when THAT flow's trigger decides it's due once asked.
// A tick much larger than a flow's intervalSeconds delays that flow firing
// on schedule; this package does not validate that relationship, the same
// way nothing in this project validates a flow's OTHER configuration choices
// for sanity before running it.
func New(flowStore *flowstore.GatedStore, buildRegistry func() (*piece.Registry, error), credStore credentials.Store, historyStore runstore.Store, tick time.Duration) *Scheduler {
	return &Scheduler{
		FlowStore: flowStore, BuildRegistry: buildRegistry, CredStore: credStore, HistoryStore: historyStore, Tick: tick,
		store: piece.NewMemoryStore(), running: map[string]bool{},
	}
}

// Run blocks, checking every saved flow once per Tick, until ctx is
// cancelled. Meant to run in its own goroutine — cmd/server does `go
// sched.Run(ctx)` alongside http.ListenAndServe.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// isScheduleTriggered reports whether fv's trigger is this project's own
// schedule piece — the only shape this scheduler acts on. Any other trigger
// (webhook, EMPTY, a JS-authored one) is silently skipped; this package has
// no opinion about how those fire.
func isScheduleTriggered(fv *model.FlowVersion) bool {
	t := fv.Trigger
	return t != nil && t.Type == model.TriggerPiece &&
		t.PieceName == schedule.PieceName && t.TriggerName == schedule.TriggerName
}

func (s *Scheduler) tick() {
	defs, err := s.FlowStore.List()
	if err != nil {
		log.Printf("scheduler: listing flows: %v", err)
		return
	}
	for _, def := range defs {
		if !isScheduleTriggered(&def.Flow) {
			continue
		}
		s.maybeRun(def)
	}
}

// maybeRun asks def's schedule trigger whether it's due and, if so, runs
// def.Flow in its own goroutine so one slow/hung run never delays checking
// the rest of the flows on this tick. Skips def entirely if a run this
// scheduler previously started for it hasn't finished yet — overlap
// protection against a flow whose intervalSeconds is shorter than how long
// it actually takes to run, not against overlap with a run started some
// other way (a manual POST /flows/{name}/run concurrent with a scheduled
// one is not this package's concern).
func (s *Scheduler) maybeRun(def flowstore.FlowDefinition) {
	s.mu.Lock()
	if s.running[def.Name] {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	scoped := &piece.ScopedStore{Underlying: s.store, FlowID: def.Name}
	items, err := schedule.Run(piece.TriggerContext{
		Input: def.Flow.Trigger.Input,
		Store: scoped,
	})
	if err != nil {
		log.Printf("scheduler: flow %q: evaluating schedule trigger: %v", def.Name, err)
		return
	}
	if len(items) == 0 {
		return
	}

	s.mu.Lock()
	s.running[def.Name] = true
	s.mu.Unlock()

	flow := def.Flow // local copy for the goroutine's own reference; flowstore.RunWithCredentials never mutates it regardless
	go func() {
		defer func() {
			s.mu.Lock()
			s.running[def.Name] = false
			s.mu.Unlock()
		}()
		for _, item := range items {
			state, _, err := flowstore.RunWithHistory(&flow, s.BuildRegistry, s.CredStore, s.HistoryStore, s.FlowStore, def.Name, item, false)
			if err != nil {
				log.Printf("scheduler: flow %q: run failed: %v", def.Name, err)
				continue
			}
			// A scheduled run is the other case with no human watching it
			// in real time — see flowstore.TriggerOnFailure's own doc
			// comment. Runs inside this same goroutine, so a slow
			// on-failure flow delays only this flow's own next tick
			// eligibility (s.running stays true until it returns), never
			// the scheduler's overall tick loop.
			flowstore.TriggerOnFailure(s.FlowStore, def.Name, def.OnFailureFlow, state, s.BuildRegistry, s.CredStore, s.HistoryStore)
		}
	}()
}
