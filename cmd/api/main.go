package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type server struct {
	db        *pgxpool.Pool
	rdb       *redis.Client
	saleStart time.Time
	productID string
}
type checkoutRequest struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
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

const rateLimitLua = `local now=tonumber(ARGV[1]); local rate=tonumber(ARGV[2]); local burst=tonumber(ARGV[3]); local v=redis.call('HMGET',KEYS[1],'tokens','ts'); local tokens=tonumber(v[1]) or burst; local ts=tonumber(v[2]) or now; tokens=math.min(burst,tokens+(now-ts)*rate); if tokens<1 then redis.call('EXPIRE',KEYS[1],60); return 0 end; redis.call('HSET',KEYS[1],'tokens',tokens-1,'ts',now); redis.call('EXPIRE',KEYS[1],60); return 1`
const admissionLua = `local n=redis.call('INCR',KEYS[1]); if n>tonumber(ARGV[1]) then redis.call('DECR',KEYS[1]); return 0 end; redis.call('EXPIRE',KEYS[1],ARGV[2]); return 1`

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
	if err != nil {
		start, _ = time.Parse(time.RFC3339, env("SALE_START", time.Now().UTC().Format(time.RFC3339)))
	}
	s := &server{db: db, rdb: rdb, saleStart: start, productID: env("PRODUCT_ID", "phone-500")}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
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
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", env("REQUEST_ID", "local-"+strconv.FormatInt(time.Now().UnixNano(), 10)))
		next.ServeHTTP(w, r)
	})
}
func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		http.Error(w, "unhealthy", 503)
		return
	}
	w.WriteHeader(204)
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
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	now := time.Now().UTC()
	if now.Before(s.saleStart) {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(response{Status: "NOT_LIVE", SaleStart: s.saleStart.Format(time.RFC3339), ServerTime: now.Format(time.RFC3339)})
		return
	}
	if ok, err := s.admit(r.Context(), r.Header.Get("X-User-ID"), r.RemoteAddr); err != nil || !ok {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(response{Status: "RATE_LIMITED", Error: "request admission limit reached"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	user := strings.TrimSpace(r.Header.Get("X-User-ID"))
	if key == "" || user == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(response{Status: "FAILED", Error: "Idempotency-Key and X-User-ID are required"})
		return
	}
	var in checkoutRequest
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.ProductID == "" || in.Quantity != 1 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(response{Status: "FAILED", Error: "product_id and quantity=1 required"})
		return
	}
	if in.ProductID != s.productID {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(response{Status: "FAILED", Error: "unknown product"})
		return
	}
	fp := sha256.Sum256([]byte(in.ProductID + ":" + strconv.Itoa(in.Quantity)))
	fingerprint := hex.EncodeToString(fp[:])
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var id, status, existingFP string
	var inserted bool
	err = tx.QueryRow(r.Context(), `INSERT INTO checkout_requests(user_id,idempotency_key,request_fingerprint,product_id,quantity,status) VALUES($1,$2,$3,$4,$5,'QUEUED') ON CONFLICT (user_id,idempotency_key) DO UPDATE SET updated_at=checkout_requests.updated_at RETURNING id,status,request_fingerprint,(xmax=0)`, user, key, fingerprint, in.ProductID, in.Quantity).Scan(&id, &status, &existingFP, &inserted)
	if err == nil && !inserted {
		if existingFP != fingerprint {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(response{Status: "FAILED", Error: "idempotency key reused with different request"})
			return
		}
		tx.Commit(r.Context())
		json.NewEncoder(w).Encode(response{TrackingID: id, Status: status, StatusURL: "/api/v1/checkout-status/" + id})
		return
	}
	if err != nil {
		http.Error(w, "database error", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO outbox_events(event_type,aggregate_id,payload) VALUES('CHECKOUT_REQUESTED',$1,$2)`, id, []byte(`{"tracking_id":"`+id+`"}`)); err != nil {
		http.Error(w, "database error", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "database error", 503)
		return
	}
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
		w.Write([]byte(v))
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

func (s *server) admit(ctx context.Context, user, remote string) (bool, error) {
	key := "flash:rate:" + strings.ReplaceAll(remote, ":", "_")
	ok, err := s.rdb.Eval(ctx, rateLimitLua, []string{key}, time.Now().UnixMilli(), 20.0/1000.0, 40).Int()
	if err != nil {
		return false, err
	}
	if ok == 0 {
		return false, nil
	}
	admitted, err := s.rdb.Eval(ctx, admissionLua, []string{"flash:admission"}, 2000, 2).Int()
	if err != nil {
		return false, err
	}
	return admitted == 1, nil
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
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", v)
		fl.Flush()
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", msg.Payload)
			fl.Flush()
		case <-t.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			fl.Flush()
		}
	}
}
