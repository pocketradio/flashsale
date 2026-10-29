package main

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rabbitmq/amqp091-go"
	"log"
	"os"
	"time"
)

type job struct {
	TrackingID string `json:"tracking_id"`
}

func main() {
	ctx := context.Background()
	db, _ := pgxpool.New(ctx, env("DATABASE_URL", "postgres://flash:flash@postgres:5432/flashsale?sslmode=disable"))
	defer db.Close()
	conn, err := amqp091.Dial(env("RABBITMQ_URL", "amqp://flash:flash@rabbitmq:5672/"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	ch, _ := conn.Channel()
	defer ch.Close()
	ch.QueueDeclare("checkout.process", true, false, false, false, amqp091.Table{"x-queue-type": "quorum"})
	ch.Qos(8, 0, false)
	go relay(ctx, db, ch)
	msgs, err := ch.Consume("checkout.process", "worker", false, false, false, false, nil)
	if err != nil {
		log.Fatal(err)
	}
	for m := range msgs {
		var j job
		if json.Unmarshal(m.Body, &j) != nil {
			m.Nack(false, false)
			continue
		}
		if err := process(ctx, db, j.TrackingID); err != nil {
			log.Printf("job %s: %v", j.TrackingID, err)
			m.Nack(false, true)
		} else {
			m.Ack(false)
		}
	}
}
func relay(ctx context.Context, db *pgxpool.Pool, ch *amqp091.Channel) {
	for {
		rows, err := db.Query(ctx, `SELECT id,aggregate_id FROM outbox_events WHERE published_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 50`)
		if err == nil {
			for rows.Next() {
				var eid int64
				var aid string
				rows.Scan(&eid, &aid)
				body, _ := json.Marshal(job{TrackingID: aid})
				if ch.PublishWithContext(ctx, "", "checkout.process", false, false, amqp091.Publishing{DeliveryMode: amqp091.Persistent, ContentType: "application/json", Body: body}) == nil {
					db.Exec(ctx, `UPDATE outbox_events SET published_at=now(),attempts=attempts+1 WHERE id=$1`, eid)
				}
			}
			rows.Close()
		}
		time.Sleep(time.Second)
	}
}
func process(ctx context.Context, db *pgxpool.Pool, id string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var product string
	var qty int
	var status string
	if err = tx.QueryRow(ctx, `SELECT product_id,quantity,status FROM checkout_requests WHERE id=$1 FOR UPDATE`, id).Scan(&product, &qty, &status); err != nil {
		return err
	}
	if status != "QUEUED" {
		return tx.Commit(ctx)
	}
	var n int
	if err = tx.QueryRow(ctx, `UPDATE inventory SET available=available-1 WHERE product_id=$1 AND available>=1 RETURNING available`, product).Scan(&n); err != nil {
		if err.Error() == "no rows in result set" {
			_, err = tx.Exec(ctx, `UPDATE checkout_requests SET status='SOLD_OUT',terminal_result='sold out',updated_at=now() WHERE id=$1`, id)
		} else {
			return err
		}
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO orders(checkout_request_id,product_id,quantity) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, product, qty)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE checkout_requests SET status='CONFIRMED',terminal_result='confirmed',updated_at=now() WHERE id=$1`, id)
		}
	}
	if err != nil {
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

var _ = time.Second
