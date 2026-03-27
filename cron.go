package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CronManager struct {
	mu     sync.Mutex
	jobs   []CronJob
	nextID int
	vpn    *VPNManager
	state  *AppState
	stopCh chan struct{}
}

func NewCronManager(vpn *VPNManager, state *AppState) *CronManager {
	return &CronManager{
		vpn:    vpn,
		state:  state,
		stopCh: make(chan struct{}),
	}
}

func (c *CronManager) Load() {
	snap := c.state.Snap()
	c.mu.Lock()
	c.jobs = snap.Cron.Jobs
	c.nextID = snap.Cron.NextID
	for i := range c.jobs {
		c.jobs[i].NextRun = nextCronRun(c.jobs[i].Expression, time.Now())
	}
	c.mu.Unlock()
	log.Printf("Loaded %d cron jobs", len(c.jobs))
}

func (c *CronManager) save() {
	c.mu.Lock()
	jobs := make([]CronJob, len(c.jobs))
	copy(jobs, c.jobs)
	nextID := c.nextID
	c.mu.Unlock()

	c.state.mu.Lock()
	c.state.sf.Cron = PersistedCron{Jobs: jobs, NextID: nextID}
	c.state.mu.Unlock()
	c.state.Save()
}

func (c *CronManager) AddJob(expr, target, action string) (*CronJob, error) {
	if err := validateCron(expr); err != nil {
		return nil, err
	}
	if action == "" {
		action = "restart"
	}
	c.mu.Lock()
	job := CronJob{
		ID:         c.nextID,
		Expression: expr,
		Target:     target,
		Action:     action,
		Enabled:    true,
		NextRun:    nextCronRun(expr, time.Now()),
	}
	c.nextID++
	c.jobs = append(c.jobs, job)
	c.mu.Unlock()
	c.save()
	return &job, nil
}

func (c *CronManager) DeleteJob(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, j := range c.jobs {
		if j.ID == id {
			c.jobs = append(c.jobs[:i], c.jobs[i+1:]...)
			go c.save()
			return nil
		}
	}
	return fmt.Errorf("job not found: %d", id)
}

func (c *CronManager) GetJobs() []CronJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]CronJob, len(c.jobs))
	copy(cp, c.jobs)
	return cp
}

func (c *CronManager) Start() {
	go c.loop()
}

func (c *CronManager) loop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case now := <-ticker.C:
			c.tick(now)
		}
	}
}

func (c *CronManager) tick(now time.Time) {
	c.mu.Lock()
	var toRun []CronJob
	for i := range c.jobs {
		if c.jobs[i].Enabled && now.After(c.jobs[i].NextRun) {
			toRun = append(toRun, c.jobs[i])
			t := now
			c.jobs[i].LastRun = &t
			c.jobs[i].NextRun = nextCronRun(c.jobs[i].Expression, now)
		}
	}
	c.mu.Unlock()
	for _, job := range toRun {
		go c.execute(job)
	}
	if len(toRun) > 0 {
		c.save()
	}
}

func (c *CronManager) execute(job CronJob) {
	c.state.AddLog("warn", fmt.Sprintf("[cron #%d] %s %s", job.ID, job.Action, job.Target))
	tunnels := c.vpn.GetTunnels()

	if job.Target == "all" {
		for _, t := range tunnels {
			switch job.Action {
			case "restart":
				c.vpn.RestartTunnel(t.Index)
			case "start":
				c.vpn.StartTunnel(t.Index)
			case "stop":
				c.vpn.StopTunnel(t.Index)
			}
			time.Sleep(200 * time.Millisecond)
		}
		return
	}

	for _, t := range tunnels {
		if job.Target != t.TunDev {
			continue
		}
		switch job.Action {
		case "restart":
			c.vpn.RestartTunnel(t.Index)
		case "start":
			c.vpn.StartTunnel(t.Index)
		case "stop":
			c.vpn.StopTunnel(t.Index)
		}
	}
}

// ── Cron expression parser ────────────────────────────────────────────────────

func validateCron(expr string) error {
	if len(strings.Fields(expr)) != 5 {
		return fmt.Errorf("cron must have 5 fields: min hour dom month dow")
	}
	return nil
}

func nextCronRun(expr string, after time.Time) time.Time {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return after.Add(time.Minute)
	}
	minute, hour, dow := fields[0], fields[1], fields[4]
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.Add(366 * 24 * time.Hour)
	for t.Before(limit) {
		if matchField(minute, t.Minute(), 0, 59) &&
			matchField(hour, t.Hour(), 0, 23) &&
			matchField(dow, int(t.Weekday()), 0, 6) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return after.Add(time.Hour)
}

func matchField(field string, value, min, max int) bool {
	if field == "*" {
		return true
	}
	if strings.HasPrefix(field, "*/") {
		n, err := strconv.Atoi(field[2:])
		if err != nil || n <= 0 {
			return false
		}
		return value%n == 0
	}
	if strings.Contains(field, "-") {
		parts := strings.SplitN(field, "-", 2)
		lo, e1 := strconv.Atoi(parts[0])
		hi, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil {
			return false
		}
		return value >= lo && value <= hi
	}
	if strings.Contains(field, ",") {
		for _, p := range strings.Split(field, ",") {
			if v, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && v == value {
				return true
			}
		}
		return false
	}
	v, err := strconv.Atoi(field)
	return err == nil && v == value
}
