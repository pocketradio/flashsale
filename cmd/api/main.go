package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type server struct {
	db           *pgxpool.Pool
	rdb          *redis.Client
	publisher    *publisher
	saleStart    time.Time
	productID    string
	requests     atomic.Uint64
	accepted     atomic.Uint64
	throttled    atomic.Uint64
	failed       atomic.Uint64
	latencyNanos atomic.Uint64
}
type checkoutRequest struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
	ClientRef string `json:"client_ref,omitempty"`
}
type response struct {
	TrackingID string `json:"tracking_id,omitempty"`
	Status     string `json:"status"`
	StatusURL  string `json:"status_url,omitempty"`
	Error      string `json:"error,omitempty"`
	SaleStart  string `json:"sale_start,omitempty"`
	ServerTime string `json:"server_time,omitempty"`
	Version    int64  `json:"version,omitempty"`
}

const admissionLua = `local t=redis.call('TIME'); local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000); if now<tonumber(ARGV[1]) then return {tostring(-4),''} end; local v=redis.call('HMGET',KEYS[1],'tokens','ts'); local tokens=tonumber(v[1]) or tonumber(ARGV[3]); local ts=tonumber(v[2]) or now; tokens=math.min(tonumber(ARGV[3]),tokens+(now-ts)*tonumber(ARGV[2])); if tokens<1 then redis.call('EXPIRE',KEYS[1],60); return {tostring(-1),''} end; redis.call('HSET',KEYS[1],'tokens',tokens-1,'ts',now); redis.call('EXPIRE',KEYS[1],60); local existing=redis.call('HGET',KEYS[3],'fingerprint'); if existing then if existing~=ARGV[5] then return {tostring(-2),''} end; return {tostring(-3),redis.call('HGET',KEYS[3],'tracking_id')} end; local n=redis.call('INCR',KEYS[2]); if n>tonumber(ARGV[4]) then redis.call('DECR',KEYS[2]); return {tostring(-1),''} end; redis.call('EXPIRE',KEYS[2],ARGV[7]); redis.call('HSET',KEYS[3],'fingerprint',ARGV[5],'tracking_id',ARGV[6]); redis.call('EXPIRE',KEYS[3],ARGV[7]); redis.call('HSET',KEYS[4],'status','QUEUED','tracking_id',ARGV[6]); redis.call('EXPIRE',KEYS[4],ARGV[7]); return {tostring(1),ARGV[6]}`
const releaseLua = `local n=tonumber(redis.call('GET',KEYS[1]) or '0'); if n>0 then redis.call('DECR',KEYS[1]) end; redis.call('DEL',KEYS[2],KEYS[3]); return 1`

func main() {
	ctx := context.Background()
	dsn := env("DATABASE_URL", "postgres://flash:flash@postgres:5432/flashsale?sslmode=disable")
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "redis:6379")})
	start, err := loadSaleStart(ctx, db)
	if err != nil || start.IsZero() {
		start, _ = time.Parse(time.RFC3339, env("SALE_START", time.Now().UTC().Format(time.RFC3339)))
	}
	p, err := newPublisher(env("RABBITMQ_URL", "amqp://flash:flash@rabbitmq:5672/"))
	if err != nil {
		log.Fatal(err)
	}
	defer p.close()
	s := &server{db: db, rdb: rdb, publisher: p, saleStart: start, productID: env("PRODUCT_ID", "phone-500")}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/api/v1/time", s.clock)
	mux.HandleFunc("/api/v1/checkout", s.checkout)
	mux.HandleFunc("/api/v1/checkout-status/", s.status)
	mux.HandleFunc("/api/v1/admin/audit", s.audit)
	addr := env("API_ADDR", ":8080")
	log.Printf("api listening on %s sale_start=%s", addr, start.Format(time.RFC3339))
	log.Fatal(http.ListenAndServe(addr, withJSON(mux)))
}
func loadSaleStart(ctx context.Context, db *pgxpool.Pool) (time.Time, error) {
	var start time.Time
	err := db.QueryRow(ctx, `SELECT sale_start FROM sale_config WHERE id=true`).Scan(&start)
	return start, err
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func withJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", env("REQUEST_ID", "local-"+strconv.FormatInt(time.Now().UnixNano(), 10)))
		next.ServeHTTP(w, r)
	})
}
func (s *server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}
func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if err := s.rdb.Ping(r.Context()).Err(); err != nil {
		http.Error(w, "redis unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ready\n"))
}
func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"requests": s.requests.Load(), "accepted_202": s.accepted.Load(),
		"throttled_429": s.throttled.Load(), "failed": s.failed.Load(),
		"average_latency_ms": func() float64 {
			n := s.requests.Load()
			if n == 0 {
				return 0
			}
			return float64(s.latencyNanos.Load()) / float64(n) / 1e6
		}(),
	})
}
func (s *server) clock(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(response{Status: "OK", SaleStart: s.saleStart.UTC().Format(time.RFC3339), ServerTime: time.Now().UTC().Format(time.RFC3339)})
}
func (s *server) audit(w http.ResponseWriter, r *http.Request) {
	var stock, confirmed, queued, outbox int
	if err := s.db.QueryRow(r.Context(), `SELECT available FROM inventory WHERE product_id=$1`, s.productID).Scan(&stock); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	s.db.QueryRow(r.Context(), `SELECT count(*) FROM orders WHERE product_id=$1`, s.productID).Scan(&confirmed)
	s.db.QueryRow(r.Context(), `SELECT count(*) FROM checkout_requests WHERE status IN ('QUEUED','PROCESSING')`).Scan(&queued)
	s.db.QueryRow(r.Context(), `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&outbox)
	json.NewEncoder(w).Encode(map[string]any{"product_id": s.productID, "database_stock": stock, "confirmed_orders": confirmed, "in_flight_requests": queued, "pending_outbox": outbox})
}
func (s *server) checkout(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	s.requests.Add(1)
	defer func() { s.latencyNanos.Add(uint64(time.Since(started))) }()
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	now := time.Now().UTC()
	if now.Before(s.saleStart) {
		s.failed.Add(1)
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(response{Status: "NOT_LIVE", SaleStart: s.saleStart.Format(time.RFC3339), ServerTime: now.Format(time.RFC3339)})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	user := strings.TrimSpace(r.Header.Get("X-User-ID"))
	if key == "" || user == "" {
		s.failed.Add(1)
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(response{Status: "FAILED", Error: "Idempotency-Key and X-User-ID are required"})
		return
	}
	var in checkoutRequest
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.ProductID == "" || in.Quantity != 1 {
		s.failed.Add(1)
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(response{Status: "FAILED", Error: "product_id and quantity=1 required"})
		return
	}
	if in.ProductID != s.productID {
		s.failed.Add(1)
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(response{Status: "FAILED", Error: "unknown product"})
		return
	}
	fp := sha256.Sum256([]byte(in.ProductID + ":" + strconv.Itoa(in.Quantity) + ":" + in.ClientRef))
	fingerprint := hex.EncodeToString(fp[:])
	id := deterministicID(user + ":" + key)
	code, ticket, err := s.admit(r.Context(), user, r.RemoteAddr, key, fingerprint, id)
	if err != nil {
		log.Printf("admission error: %v", err)
		http.Error(w, "admission unavailable", 503)
		return
	}
	if code == -4 {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(response{Status: "NOT_LIVE", SaleStart: s.saleStart.Format(time.RFC3339), ServerTime: time.Now().UTC().Format(time.RFC3339)})
		return
	}
	if code == -1 {
		s.throttled.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(response{Status: "RATE_LIMITED", Error: "request admission limit reached"})
		return
	}
	if code == -2 {
		s.failed.Add(1)
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(response{Status: "FAILED", Error: "idempotency key reused with different request"})
		return
	}
	if code == -3 {
		id = ticket
	}
	if code == 1 {
		body, _ := json.Marshal(job{TrackingID: id, UserID: user, ProductID: in.ProductID, Quantity: in.Quantity, IdempotencyKey: key, Fingerprint: fingerprint, CreatedAt: time.Now().UTC()})
		if err := s.publisher.publish(r.Context(), body); err != nil {
			s.failed.Add(1)
			s.releaseAdmission(r.Context(), user, key, id)
			http.Error(w, "queue unavailable", 503)
			return
		}
	}
	s.accepted.Add(1)
	w.WriteHeader(202)
	json.NewEncoder(w).Encode(response{TrackingID: id, Status: "QUEUED", StatusURL: "/api/v1/checkout-status/" + id})
}
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/checkout-status/")
	if strings.HasSuffix(id, "/stream") {
		s.stream(w, r, strings.TrimSuffix(id, "/stream"))
		return
	}
	if id == "" {
		http.Error(w, "missing tracking id", 400)
		return
	}
	if v, err := s.rdb.Get(r.Context(), "checkout:status:"+id).Result(); err == nil {
		w.Header().Set("X-Status-Source", "redis")
		w.Write([]byte(v))
		return
	}
	if v, err := s.rdb.HGetAll(r.Context(), "flash:ticket:"+id).Result(); err == nil && len(v) > 0 {
		w.Header().Set("X-Status-Source", "redis-ticket")
		b, _ := json.Marshal(response{TrackingID: id, Status: v["status"]})
		w.Write(b)
		return
	}
	var status string
	var result []byte
	err := s.db.QueryRow(r.Context(), `SELECT status,terminal_result FROM checkout_requests WHERE id=$1`, id).Scan(&status, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	out := response{TrackingID: id, Status: status}
	if len(result) > 0 {
		out.Error = string(result)
	}
	b, _ := json.Marshal(out)
	s.rdb.Set(r.Context(), "checkout:status:"+id, b, 10*time.Minute)
	w.Write(b)
}

func (s *server) admit(ctx context.Context, user, remote, idemKey, fingerprint, id string) (int, string, error) {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	rateKey := "flash:rate:" + strings.ReplaceAll(remote, ":", "_")
	rateRPS, _ := strconv.ParseFloat(env("CHECKOUT_RATE_RPS", "20"), 64)
	res, err := s.rdb.Eval(ctx, admissionLua, []string{rateKey, "flash:admission", "flash:idem:" + user + ":" + idemKey, "flash:ticket:" + id}, s.saleStart.UnixMilli(), rateRPS/1000.0, int(rateRPS*2), 2000, fingerprint, id, 30).StringSlice()
	if err != nil {
		return 0, "", err
	}
	code, err := strconv.Atoi(res[0])
	if err != nil {
		return 0, "", err
	}
	return code, res[1], nil
}

func (s *server) releaseAdmission(ctx context.Context, user, idemKey, id string) {
	s.rdb.Eval(ctx, releaseLua, []string{"flash:admission", "flash:idem:" + user + ":" + idemKey, "flash:ticket:" + id})
}

func (s *server) stream(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", 500)
		return
	}
	pubsub := s.rdb.Subscribe(r.Context(), "checkout:events:"+id)
	defer pubsub.Close()
	ch := pubsub.Channel()
	if v, err := s.rdb.Get(r.Context(), "checkout:status:"+id).Result(); err == nil {
		fmt.Fprintf(w, "id: 0\nevent: status\ndata: %s\n\n", v)
		fl.Flush()
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "id: %d\nevent: status\ndata: %s\n\n", time.Now().UnixNano(), msg.Payload)
			fl.Flush()
		case <-t.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			fl.Flush()
		}
	}
}
