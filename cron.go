package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CronJob struct {
	ID         int    `json:"id"`
	Expression string `json:"expression"`
	Target     string `json:"target"` // "all" or "tun0", "tun1", ...
	Action     string `json:"action"` // "restart" | "start" | "stop"
	Enabled    bool   `json:"enabled"`
	NextRun    time.Time `json:"next_run"`
	LastRun    *time.Time `json:"last_run,omitempty"`
}

type CronState struct {
	Jobs    []CronJob `json:"jobs"`
	NextID  int       `json:"next_id"`
}

type CronManager struct {
	mu      sync.Mutex
	jobs    []CronJob
	nextID  int
	dataDir string
	vpn     *VPNManager
	stopCh  chan struct{}
}

func NewCronManager(vpn *VPNManager) *CronManager {
	return &CronManager{
		vpn:    vpn,
		stopCh: make(chan struct{}),
	}
}

func (c *CronManager) SetDataDir(dir string) {
	c.mu.Lock()
	c.dataDir = dir
	c.mu.Unlock()
}

func (c *CronManager) stateFile() string {
	return filepath.Join(c.vpn.dataDir, "cron.json")
}

func (c *CronManager) SaveState() {
	c.mu.Lock()
	defer c.mu.Unlock()

	cs := CronState{Jobs: c.jobs, NextID: c.nextID}
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(c.stateFile(), data, 0644)
}

func (c *CronManager) LoadState() {
	data, err := os.ReadFile(c.stateFile())
	if err != nil {
		return
	}
	var cs CronState
	if err := json.Unmarshal(data, &cs); err != nil {
		log.Printf("cron LoadState: %v", err)
		return
	}
	c.mu.Lock()
	c.jobs = cs.Jobs
	c.nextID = cs.NextID
	// recalculate next runs
	for i := range c.jobs {
		c.jobs[i].NextRun = nextCronRun(c.jobs[i].Expression, time.Now())
	}
	c.mu.Unlock()
	log.Printf("Loaded %d cron jobs", len(c.jobs))
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

	c.SaveState()
	return &job, nil
}

func (c *CronManager) DeleteJob(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, j := range c.jobs {
		if j.ID == id {
			c.jobs = append(c.jobs[:i], c.jobs[i+1:]...)
			go c.SaveState()
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
		c.execute(job)
	}
	if len(toRun) > 0 {
		c.SaveState()
	}
}

func (c *CronManager) execute(job CronJob) {
	tunnels := c.vpn.GetTunnels()
	c.vpn.addLog("warn", fmt.Sprintf("[cron #%d] %s %s", job.ID, job.Action, job.Target))

	for _, t := range tunnels {
		if job.Target != "all" && job.Target != t.TunDev {
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

// ── Cron expression parser (5-field standard) ─────────────────────────────────

func validateCron(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("cron expression must have 5 fields (min hour dom month dow)")
	}
	return nil
}

// nextCronRun returns the next time after `after` that matches the cron expression.
func nextCronRun(expr string, after time.Time) time.Time {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return after.Add(time.Minute)
	}

	minute := fields[0]
	hour := fields[1]
	// dom := fields[2]  (simplified: we match * and /n)
	// month := fields[3]
	dow := fields[4]

	// start from next minute
	t := after.Truncate(time.Minute).Add(time.Minute)

	// search up to 1 year
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
	// */n
	if strings.HasPrefix(field, "*/") {
		n, err := strconv.Atoi(field[2:])
		if err != nil || n <= 0 {
			return false
		}
		return value%n == 0
	}
	// a-b
	if strings.Contains(field, "-") {
		parts := strings.SplitN(field, "-", 2)
		lo, e1 := strconv.Atoi(parts[0])
		hi, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil {
			return false
		}
		return value >= lo && value <= hi
	}
	// a,b,c
	if strings.Contains(field, ",") {
		for _, p := range strings.Split(field, ",") {
			if v, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && v == value {
				return true
			}
		}
		return false
	}
	// literal
	v, err := strconv.Atoi(field)
	if err != nil {
		return false
	}
	return v == value
}
