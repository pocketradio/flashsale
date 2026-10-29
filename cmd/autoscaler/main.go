package main

// The local controller is intentionally vendor-neutral. It observes the Docker
// Compose deployment and changes API replicas with `docker compose --scale`.
// Production maps the same policy to Kubernetes HPA or ECS service scaling.
import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type sample struct {
	CPU              float64
	QueueAge         time.Duration
	QueueDepth       int
	Current, Healthy int
}

func desired(s sample, min, max int, targetCPU float64, queueAge time.Duration) int {
	n := s.Current
	if n < min {
		n = min
	}
	if n == 0 {
		n = min
	}
	if s.QueueAge > queueAge || s.QueueDepth > 0 || s.CPU > targetCPU {
		n += 2
	}
	if s.QueueAge == 0 && s.QueueDepth == 0 && s.CPU < targetCPU*0.35 && n > min {
		n--
	}
	if n > max {
		n = max
	}
	if n < min {
		n = min
	}
	return n
}

func main() {
	interval := flag.Duration("interval", envDuration("AUTOSCALER_INTERVAL", 10*time.Second), "sampling interval")
	min := flag.Int("min", envInt("MIN_API_REPLICAS", 4), "minimum API replicas")
	max := flag.Int("max", envInt("MAX_API_REPLICAS", 8), "maximum API replicas")
	target := flag.Float64("target-cpu", envFloat("TARGET_CPU_PERCENT", 50), "target CPU percentage")
	queueAge := flag.Duration("queue-age", envDuration("QUEUE_AGE_SCALE_UP", 2*time.Second), "queue age scale-up threshold")
	stabilize := flag.Duration("scale-down-stabilization", envDuration("SCALE_DOWN_STABILIZATION", 60*time.Second), "scale-down stabilization")
	once := flag.Bool("once", false, "evaluate once and exit")
	dryRun := flag.Bool("dry-run", false, "do not change replicas")
	flag.Parse()
	log.Printf("autoscaler started min=%d max=%d target_cpu=%.1f queue_age=%s", *min, *max, *target, *queueAge)
	lastScale := time.Time{}
	for {
		s := observe()
		d := desired(s, *min, *max, *target, *queueAge)
		if d < s.Current && !lastScale.IsZero() && time.Since(lastScale) < *stabilize {
			d = s.Current
		}
		log.Printf(`{"component":"autoscaler","current":%d,"healthy":%d,"cpu_percent":%.2f,"queue_depth":%d,"queue_age":"%s","desired":%d}`, s.Current, s.Healthy, s.CPU, s.QueueDepth, s.QueueAge, d)
		if d != s.Current {
			if *dryRun {
				log.Printf("dry-run scale desired=%d", d)
			} else {
				if err := scale(d); err != nil {
					log.Printf("scale failed desired=%d error=%v", d, err)
				} else {
					lastScale = time.Now()
					refreshUpstream()
					log.Printf("scale applied desired=%d", d)
				}
			}
		} else {
			refreshUpstream()
		}
		if *once {
			return
		}
		time.Sleep(*interval)
	}
}

func observe() sample {
	// Compose ps is portable across Docker Desktop and Linux Docker Engine.
	out, err := exec.Command("docker", "compose", "ps", "-q", "api").Output()
	if err != nil {
		return sample{Current: 0}
	}
	var ids []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			ids = append(ids, strings.TrimSpace(sc.Text()))
		}
	}
	healthy := 0
	for _, id := range ids {
		if ready(id) {
			healthy++
		}
	}
	return sample{Current: len(ids), Healthy: healthy, CPU: apiCPU(ids), QueueDepth: queueDepth()}
}

func apiCPU(ids []string) float64 {
	out, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.ID}}|{{.CPUPerc}}").Output()
	if err != nil {
		return 0
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id[:min(12, len(id))]] = true
	}
	var total float64
	var n int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.Split(line, "|")
		if len(p) != 2 || !want[p[0]] {
			continue
		}
		v := strings.TrimSuffix(strings.TrimSpace(p[1]), "%")
		if x, e := strconv.ParseFloat(v, 64); e == nil {
			total += x
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return total / float64(n)
}
func queueDepth() int {
	// RabbitMQ exposes durable queue depth through its management endpoint.
	// Queue age is emitted by the worker; a non-empty queue is treated as
	// pressure for local scaling when that age signal is unavailable.
	out, err := exec.Command("docker", "compose", "exec", "-T", "rabbitmq", "rabbitmqctl", "list_queues", "-q", "name", "messages_ready").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		p := strings.Fields(line)
		if len(p) == 2 && p[0] == "checkout.process" {
			n, _ := strconv.Atoi(p[1])
			return n
		}
	}
	return 0
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func ready(id string) bool {
	if out, err := exec.Command("docker", "inspect", "-f", "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", id).Output(); err == nil {
		status := strings.TrimSpace(string(out))
		if status == "healthy" {
			return true
		}
		if status == "starting" || status == "unhealthy" {
			return false
		}
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id).Output()
	if err != nil {
		return false
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return false
	}
	c, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(c, http.MethodGet, "http://"+ip+":8080/readyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func scale(n int) error {
	cmd := exec.Command("docker", "compose", "up", "-d", "--no-recreate", "--scale", "api="+strconv.Itoa(n), "api")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
func refreshUpstream() {
	out, err := exec.Command("docker", "compose", "ps", "-q", "api").Output()
	if err != nil {
		return
	}
	var servers []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		id := strings.TrimSpace(sc.Text())
		if id == "" || !ready(id) {
			continue
		}
		ip, err := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id).Output()
		if err == nil && strings.TrimSpace(string(ip)) != "" {
			servers = append(servers, "server "+strings.TrimSpace(string(ip))+":8080;")
		}
	}
	if len(servers) == 0 {
		return
	}
	path := env("NGINX_UPSTREAM_FILE", "nginx-upstream.conf")
	tmp := path + ".tmp"
	data := "upstream api_pool_dynamic { least_conn; " + strings.Join(servers, " ") + " keepalive 128; }\n"
	if os.WriteFile(tmp, []byte(data), 0644) == nil {
		_ = os.Rename(tmp, path)
		_ = exec.Command("docker", "compose", "exec", "-T", "nginx", "nginx", "-s", "reload").Run()
	}
}
func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return d
}
func envFloat(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil && v > 0 {
		return v
	}
	return d
}
func envDuration(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if x, err := time.ParseDuration(v); err == nil {
			return x
		}
	}
	return d
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var _ = fmt.Sprintf
var _ = json.Valid
