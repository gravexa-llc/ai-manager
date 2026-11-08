package sessions_test

import (
	"context"
	"fmt"
	"log"

	"github.com/gravexa-llc/ai-manager/internal/sessions"
)

// How aimd is expected to wire the poller up. This example is compiled
// but not run: it would call the real claude.
func Example_daemon() {
	ctx := context.Background()

	p := sessions.NewPoller(sessions.Options{
		Interval: sessions.IntervalUnwatched,
		OnResult: func(r sessions.Result) {
			if r.Err != nil {
				log.Printf("sessions: %v", r.Err) // nothing is marked gone
				return
			}
			for _, e := range r.Events {
				if e.Kind == sessions.Gone {
					fmt.Println("gone:", e.Key)
					continue
				}
				if v := e.After.Verdict(); v.NeedsYou() && e.VerdictChanged() {
					fmt.Println("needs you:", e.Key, v.Need, v.Reason)
				}
			}
		},
	})

	// File changes in the session registry and job store kick a poll.
	if dirs, err := sessions.WatchDirs(); err == nil {
		go func() {
			if err := sessions.Watch(ctx, dirs, p.Kick); err != nil {
				log.Printf("sessions: %v", err) // polling still works
			}
		}()
	}

	// When a dashboard viewer connects: p.SetInterval(sessions.IntervalWatched).
	// Once the mod reports live: p.SetInterval(sessions.IntervalModLive).
	_ = p.Run(ctx)
}

func ExampleSession_Verdict() {
	snap, err := sessions.Parse([]byte(`[
	  {"id": "199dc94c", "pid": 69310, "kind": "background", "status": "idle", "state": "blocked"},
	  {"pid": 10006, "kind": "interactive", "status": "waiting", "waitingFor": "input needed"},
	  {"pid": 91470, "kind": "interactive", "status": "busy"}
	]`))
	if err != nil {
		log.Fatal(err)
	}
	for _, s := range snap.Sessions {
		fmt.Println(s.Key(), s.Verdict())
	}
	// Output:
	// job:199dc94c needs_you (reply: reply)
	// pid:10006 needs_you (prompt: input needed)
	// pid:91470 working (busy)
}
