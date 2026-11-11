package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	amqp "github.com/rabbitmq/amqp091-go"
)

const queueName = "image.jobs.v2"

var completed = promauto.NewCounterVec(prometheus.CounterOpts{Name: "image_job_attempts_total", Help: "Completed attempts by outcome."}, []string{"outcome"})
var processing = promauto.NewHistogram(prometheus.HistogramOpts{Name: "image_job_processing_seconds", Help: "Worker processing time per attempt."})
var turnaround = promauto.NewHistogram(prometheus.HistogramOpts{Name: "image_job_turnaround_seconds", Help: "Time from acceptance to success.", Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}})
var pending = promauto.NewGauge(prometheus.GaugeOpts{Name: "image_jobs_pending", Help: "Jobs queued or processing."})
var depth = promauto.NewGauge(prometheus.GaugeOpts{Name: "image_broker_ready_messages", Help: "RabbitMQ messages available for delivery."})
var oldest = promauto.NewGauge(prometheus.GaugeOpts{Name: "image_oldest_pending_seconds", Help: "Age of oldest pending job."})

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *service) brokerLoop(ctx context.Context, handle func(context.Context, *amqp.Channel) error) error {
	for ctx.Err() == nil {
		if err := s.brokerSession(ctx, handle); err != nil && ctx.Err() == nil {
			slog.Warn("broker session interrupted", "error", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return ctx.Err()
}

func (s *service) brokerSession(ctx context.Context, handle func(context.Context, *amqp.Channel) error) error {
	connection, err := amqp.DialConfig(s.broker, amqp.Config{Dial: amqp.DefaultDial(5 * time.Second)})
	if err != nil {
		return err
	}
	defer connection.CloseDeadline(time.Now().Add(2 * time.Second))
	channel, err := connection.Channel()
	if err != nil {
		return err
	}
	if _, err = channel.QueueDeclare(queueName, true, false, false, false, nil); err != nil {
		return err
	}
	return handle(ctx, channel)
}

func (s *service) dispatch(ctx context.Context, ch *amqp.Channel) error {
	if err := ch.Confirm(false); err != nil {
		return err
	}
	returned := ch.NotifyReturn(make(chan amqp.Return, 1))
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	nextSweep := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if time.Now().After(nextSweep) {
			sweepCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := s.sweep(sweepCtx)
			cancel()
			if err != nil {
				return err
			}
			queue, err := ch.QueueInspect(queueName)
			if err != nil {
				return err
			}
			depth.Set(float64(queue.Messages))
			nextSweep = time.Now().Add(2 * time.Second)
		}
		for range 100 {
			publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			published, err := s.publishOne(publishCtx, ch, returned)
			cancel()
			if err != nil {
				return err
			}
			if !published {
				break
			}
		}
	}
}

func (s *service) sweep(ctx context.Context) error {
	if err := s.recoverExpired(ctx); err != nil {
		return err
	}
	var count, age float64
	if err := s.db.QueryRow(ctx, `SELECT count(*)::float8,COALESCE(EXTRACT(EPOCH FROM now()-min(created_at)),0)::float8
 FROM image_jobs WHERE status IN ('queued','processing')`).Scan(&count, &age); err != nil {
		return err
	}
	pending.Set(count)
	oldest.Set(age)
	return nil
}

func (s *service) publishOne(ctx context.Context, ch *amqp.Channel, returned <-chan amqp.Return) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT job_id::text FROM image_outbox
 WHERE published_at IS NULL AND available_at<=now() ORDER BY available_at LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
	if missing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	confirmation, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", queueName, true, false,
		amqp.Publishing{DeliveryMode: amqp.Persistent, ContentType: "text/plain", MessageId: id, Body: []byte(id)})
	if err != nil {
		return false, err
	}
	ack, err := confirmation.WaitContext(ctx)
	if err != nil {
		return false, err
	}
	if !ack {
		return false, fmt.Errorf("broker rejected job %s", id)
	}
	select {
	case message, open := <-returned:
		if !open {
			return false, fmt.Errorf("broker channel closed during publish")
		}
		return false, fmt.Errorf("broker returned job %s: %s", id, message.ReplyText)
	default:
	}
	if _, err := tx.Exec(ctx, "UPDATE image_outbox SET published_at=now() WHERE job_id=$1::text::uuid", id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *service) consume(ctx context.Context, ch *amqp.Channel) error {
	if err := ch.Qos(1, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(queueName, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case delivery, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("broker closed the consumer")
			}
			id := string(delivery.Body)
			if uuidPattern.MatchString(id) {
				// A shutdown lets the current attempt finish. A hard stop is recovered by the dispatcher.
				attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.lease-500*time.Millisecond)
				err := s.process(attemptCtx, id)
				cancel()
				if err != nil {
					return err
				} // closing the connection releases the unacknowledged message
			}
			if err := delivery.Ack(false); err != nil {
				return err
			}
		}
	}
}

func (s *service) process(ctx context.Context, id string) error {
	a, err := s.claim(ctx, id, newID())
	if missing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	start := time.Now()
	slog.Info("job claimed", "task_id", id, "attempt", a.count)
	result := "result/" + id + "/" + a.token
	path := "/render?" + url.Values{"source": {a.source}, "result": {result}, "filter": {a.filter}}.Encode()
	response, err := s.processorRequest(ctx, "POST", path, nil)
	status, code := "succeeded", ""
	if response != nil {
		response.Body.Close()
	}
	switch {
	case err == nil && response.StatusCode == http.StatusNoContent:
	case err == nil && response.StatusCode == http.StatusUnprocessableEntity:
		status, code = "failed", "invalid_image"
	case a.count >= maxAttempts:
		status, code = "failed", "attempts_exhausted"
	default:
		status, code = "queued", "processor_unavailable"
	}
	if status != "succeeded" {
		result = ""
	}
	committed, err := s.finish(ctx, a, status, result, code)
	if err != nil {
		return err
	}
	if !committed {
		if result != "" {
			s.cleanupObject(ctx, result)
		}
		return nil
	}
	processing.Observe(time.Since(start).Seconds())
	completed.WithLabelValues(status).Inc()
	if status == "succeeded" {
		turnaround.Observe(time.Since(a.created).Seconds())
	}
	slog.Info("job attempt finished", "task_id", id, "attempt", a.count, "status", status, "error_code", code)
	return nil
}
