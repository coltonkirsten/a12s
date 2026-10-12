// Command svc-cron is the reference cron Service: it
// enqueues on a schedule with the send grant the Fleet gives it. A slot's
// idempotency key is <job>/<slot>, so a double fire enqueues once; a job's
// replace becomes the replace key and its expires the deadline; on each fire
// it reads back the job's previous Message and logs it if it expired unread.
// Slots missed while asleep are skipped, not replayed.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/coltonkirsten/a12s/services/svckit"
)

type job struct {
	Name     string            `json:"name"`
	Cron     string            `json:"cron"`
	To       string            `json:"to"`
	Body     string            `json:"body"`
	Fresh    bool              `json:"fresh,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Expires  string            `json:"expires,omitempty"`
	Replace  string            `json:"replace,omitempty"`

	sched   *Schedule
	expires *int64
	last    string // the previous fire's Message id
}

type config struct {
	Jobs []*job `json:"jobs"`
	// TZ is the zone schedules are read in (default UTC).
	TZ string `json:"tz,omitempty"`
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stdout)
	var cfg config
	if err := svckit.LoadConfig(&cfg); err != nil {
		log.Fatalf("svc-cron: config: %v", err)
	}
	loc := time.UTC
	if cfg.TZ != "" {
		l, err := time.LoadLocation(cfg.TZ)
		if err != nil {
			log.Fatalf("svc-cron: tz: %v", err)
		}
		loc = l
	}
	for _, j := range cfg.Jobs {
		s, err := ParseSchedule(j.Cron)
		if err != nil {
			log.Fatalf("svc-cron: job %s: %v", j.Name, err)
		}
		j.sched = s
		if j.Expires != "" {
			d, err := svckit.ParseDuration(j.Expires)
			if err != nil {
				log.Fatalf("svc-cron: job %s: expires: %v", j.Name, err)
			}
			n := int64(d / time.Second)
			j.expires = &n
		}
		if j.Name == "" || j.To == "" || j.Body == "" {
			log.Fatalf("svc-cron: every job needs name, to and body")
		}
	}
	client, err := svckit.NewClient()
	if err != nil {
		log.Fatalf("svc-cron: %v", err)
	}
	mux := http.NewServeMux()
	svckit.Health(mux)
	go func() { log.Fatal(http.ListenAndServe(":8080", mux)) }()
	log.Printf("svc-cron: %d job(s), schedules in %s", len(cfg.Jobs), loc)

	next := time.Now().Truncate(time.Minute).Add(time.Minute)
	for {
		time.Sleep(time.Until(next))
		slot := next.In(loc)
		// A slot more than a minute late (the Service was asleep or
		// paused) is skipped, not replayed.
		if time.Since(next) < time.Minute {
			for _, j := range cfg.Jobs {
				if j.sched.Match(slot) {
					fire(client, j, slot)
				}
			}
		}
		next = time.Now().Truncate(time.Minute).Add(time.Minute)
	}
}

func fire(c *svckit.Client, j *job, slot time.Time) {
	if j.last != "" {
		if prev, err := c.Get(j.To, j.last); err == nil && prev.State == "expired" {
			log.Printf("%s %s expired unread", j.Name, prev.ID)
		}
	}
	key := fmt.Sprintf("%s/%s", j.Name, slot.UTC().Format("2006-01-02T15:04Z"))
	md := map[string]string{"job": j.Name}
	for k, v := range j.Metadata {
		md[k] = v
	}
	m, code, err := c.Send(j.To, svckit.Enqueue{
		Body: j.Body, Sender: "cron:" + j.Name, Metadata: md, Fresh: j.Fresh,
		IdempotencyKey: key, Expires: j.expires, ReplaceKey: j.Replace,
	})
	var ae *svckit.APIError
	switch {
	case errors.As(err, &ae):
		log.Printf("%s to %s refused: %v", j.Name, j.To, ae)
		return
	case err != nil:
		log.Printf("%s to %s failed: %v", j.Name, j.To, err)
		return
	}
	j.last = m.ID
	extra := ""
	if m.Replaced != "" {
		extra = " (replaced " + m.Replaced + ")"
	}
	log.Printf("%s enqueued to %s: %s %s (%d, key %s)%s", j.Name, j.To, m.ID, m.State, code, key, extra)
}
