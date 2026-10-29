package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

type job struct {
	TrackingID     string    `json:"tracking_id"`
	UserID         string    `json:"user_id"`
	ProductID      string    `json:"product_id"`
	Quantity       int       `json:"quantity"`
	IdempotencyKey string    `json:"idempotency_key"`
	Fingerprint    string    `json:"request_fingerprint"`
	CreatedAt      time.Time `json:"created_at"`
}

func main() {
	ctx := context.Background()
	db, _ := pgxpool.New(ctx, env("DATABASE_URL", "postgres://flash:flash@postgres:5432/flashsale?sslmode=disable"))
	defer db.Close()
	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "redis:6379")})
	defer rdb.Close()
	conn, err := amqp091.Dial(env("RABBITMQ_URL", "amqp://flash:flash@rabbitmq:5672/"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	ch, _ := conn.Channel()
	defer ch.Close()
	ch.ExchangeDeclare("checkout.dlx", "direct", true, false, false, false, nil)
	ch.QueueDeclare("checkout.dead", true, false, false, false, amqp091.Table{"x-queue-type": "quorum"})
	ch.QueueBind("checkout.dead", "checkout.process", "checkout.dlx", false, nil)
	ch.QueueDeclare("checkout.process", true, false, false, false, amqp091.Table{"x-queue-type": "quorum", "x-dead-letter-exchange": "checkout.dlx"})
	ch.Confirm(false)
	confirms := ch.NotifyPublish(make(chan amqp091.Confirmation, 128))
	prefetch := envInt("WORKER_PREFETCH", 64)
	ch.Qos(prefetch, 0, false)
	go relay(ctx, db, ch, confirms)
	msgs, err := ch.Consume("checkout.process", "worker", false, false, false, false, nil)
	if err != nil {
		log.Fatal(err)
	}
	concurrency := envInt("WORKER_CONCURRENCY", prefetch)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for m := range msgs {
		sem <- struct{}{}
		wg.Add(1)
		go func(m amqp091.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			var j job
			if json.Unmarshal(m.Body, &j) != nil {
				m.Nack(false, false)
				return
			}
			if err := process(ctx, db, j); err != nil {
				log.Printf("job %s: %v", j.TrackingID, err)
				m.Nack(false, true)
				return
			}
			syncStatus(ctx, db, rdb, j.TrackingID)
			m.Ack(false)
		}(m)
	}
	wg.Wait()
}
func syncStatus(ctx context.Context, db *pgxpool.Pool, rdb *redis.Client, id string) {
	var status, result string
	var version int64
	if db.QueryRow(ctx, `SELECT status,COALESCE(terminal_result,''),status_version FROM checkout_requests WHERE id=$1`, id).Scan(&status, &result, &version) == nil {
		b, _ := json.Marshal(map[string]any{"tracking_id": id, "status": status, "result": result, "version": version})
		rdb.Set(ctx, "checkout:status:"+id, b, 10*time.Minute)
		rdb.Publish(ctx, "checkout:events:"+id, b)
		db.Exec(ctx, `UPDATE outbox_events SET published_at=now(),attempts=attempts+1 WHERE aggregate_id=$1 AND event_type='CHECKOUT_STATUS_UPDATED' AND published_at IS NULL`, id)
	}
}
func relay(ctx context.Context, db *pgxpool.Pool, ch *amqp091.Channel, confirms <-chan amqp091.Confirmation) {
	for {
		rows, err := db.Query(ctx, `SELECT id,aggregate_id FROM outbox_events WHERE event_type='CHECKOUT_REQUESTED' AND published_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 50`)
		if err == nil {
			for rows.Next() {
				var eid int64
				var aid string
				rows.Scan(&eid, &aid)
				body, _ := json.Marshal(job{TrackingID: aid})
				if ch.PublishWithContext(ctx, "", "checkout.process", false, false, amqp091.Publishing{DeliveryMode: amqp091.Persistent, ContentType: "application/json", Body: body}) == nil {
					confirmed := <-confirms
					if !confirmed.Ack {
						continue
					}
					db.Exec(ctx, `UPDATE outbox_events SET published_at=now(),attempts=attempts+1 WHERE id=$1`, eid)
				}
			}
			rows.Close()
		}
		time.Sleep(time.Second)
	}
}
func process(ctx context.Context, db *pgxpool.Pool, j job) error {
	id := j.TrackingID
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var inserted string
	err = tx.QueryRow(ctx, `INSERT INTO checkout_requests(id,user_id,idempotency_key,request_fingerprint,product_id,quantity,status) VALUES($1,$2,$3,$4,$5,$6,'QUEUED') ON CONFLICT (user_id,idempotency_key) DO NOTHING RETURNING id`, id, j.UserID, j.IdempotencyKey, j.Fingerprint, j.ProductID, j.Quantity).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		var existingFP, existingStatus string
		if err = tx.QueryRow(ctx, `SELECT request_fingerprint,status FROM checkout_requests WHERE user_id=$1 AND idempotency_key=$2`, j.UserID, j.IdempotencyKey).Scan(&existingFP, &existingStatus); err != nil {
			return err
		}
		if existingFP != j.Fingerprint {
			return tx.Commit(ctx)
		}
		if existingStatus == "CONFIRMED" || existingStatus == "SOLD_OUT" || existingStatus == "FAILED" {
			return tx.Commit(ctx)
		}
	} else if err != nil {
		return err
	}
	tokenBytes := make([]byte, 16)
	if _, err = rand.Read(tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	var product string
	var qty int
	err = tx.QueryRow(ctx, `UPDATE checkout_requests SET status='PROCESSING',processing_token=$2,lease_until=now()+interval '30 seconds',updated_at=now() WHERE id=$1 AND (status='QUEUED' OR (status='PROCESSING' AND lease_until<now())) RETURNING product_id,quantity`, id, token).Scan(&product, &qty)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	var n int
	if err = tx.QueryRow(ctx, `UPDATE inventory SET available=available-1 WHERE product_id=$1 AND available>=1 RETURNING available`, product).Scan(&n); err != nil {
		if err.Error() == "no rows in result set" {
			_, err = tx.Exec(ctx, `UPDATE checkout_requests SET status='SOLD_OUT',terminal_result='sold out',status_version=status_version+1,processing_token=NULL,lease_until=NULL,updated_at=now() WHERE id=$1 AND processing_token=$2`, id, token)
		} else {
			return err
		}
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO orders(checkout_request_id,product_id,quantity) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, product, qty)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE checkout_requests SET status='CONFIRMED',terminal_result='confirmed',status_version=status_version+1,processing_token=NULL,lease_until=NULL,updated_at=now() WHERE id=$1 AND processing_token=$2`, id, token)
		}
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(event_type,aggregate_id,payload) VALUES('CHECKOUT_STATUS_UPDATED',$1,$2)`, id, []byte(`{"tracking_id":"`+id+`"}`)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	v, err := strconv.Atoi(env(k, strconv.Itoa(d)))
	if err != nil || v < 1 {
		return d
	}
	return v
}

var _ = time.Second
