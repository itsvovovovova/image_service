package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/bcrypt"
)

const maxImageBytes = 10 << 20

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,64}$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var filters = map[string]bool{"Negative": true, "Black & White": true, "Soft Blur": true, "Sharpen Details": true,
	"Sketch Outline": true, "Contour Drawing": true, "Emboss Effect": true, "Poster Style": true, "Photo Negative": true}

func (s *service) serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /register", s.credentials(true))
	mux.HandleFunc("POST /login", s.credentials(false))
	mux.HandleFunc("POST /task", s.auth(s.upload))
	mux.HandleFunc("GET /status/{id}", s.auth(s.status))
	mux.HandleFunc("GET /result/{id}", s.auth(s.result))
	server := &http.Server{Addr: ":8007", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}

}

func (s *service) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var schema bool
	if err := s.db.QueryRow(ctx, "SELECT to_regclass('image_outbox') IS NOT NULL").Scan(&schema); err != nil || !schema {
		fail(w, 503, "database_unavailable")
		return
	}
	response, err := s.processorRequest(ctx, "GET", "/readyz", nil)
	if err != nil {
		fail(w, 503, "storage_unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 204 {
		fail(w, 503, "storage_unavailable")
		return
	}
	w.WriteHeader(204)
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, code string) {
	respond(w, status, map[string]string{"error": code})
}

func (s *service) credentials(register bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || !usernamePattern.MatchString(input.Username) ||
			len(input.Password) < 8 || len(input.Password) > 72 {
			fail(w, 400, "invalid_credentials")
			return
		}
		if register {
			hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
			if err != nil {
				fail(w, 500, "internal_error")
				return
			}
			_, err = s.db.Exec(r.Context(), "INSERT INTO image_users(username,password_hash) VALUES($1,$2)", input.Username, string(hash))
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				fail(w, 409, "username_taken")
				return
			}
			if err != nil {
				fail(w, 503, "database_unavailable")
				return
			}
			respond(w, 201, map[string]string{"detail": "User created"})
			return
		}
		var hash string
		err := s.db.QueryRow(r.Context(), "SELECT password_hash FROM image_users WHERE username=$1", input.Username).Scan(&hash)
		if err != nil && !missing(err) {
			fail(w, 503, "database_unavailable")
			return
		}
		if missing(err) || bcrypt.CompareHashAndPassword([]byte(hash), []byte(input.Password)) != nil {
			fail(w, 401, "invalid_credentials")
			return
		}
		claims := jwt.RegisteredClaims{Subject: input.Username, Issuer: "image-service", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
		if err != nil {
			fail(w, 500, "internal_error")
			return
		}
		respond(w, 200, map[string]string{"token": token})
	}
}

type userHandler func(http.ResponseWriter, *http.Request, string)

func (s *service) auth(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || raw == "" {
			fail(w, 401, "unauthorized")
			return
		}
		claims := new(jwt.RegisteredClaims)
		token, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return s.secret, nil },
			jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(), jwt.WithIssuer("image-service"))
		if err != nil || !token.Valid || claims.Subject == "" {
			fail(w, 401, "unauthorized")
			return
		}
		next(w, r, claims.Subject)
	}
}

func (s *service) upload(w http.ResponseWriter, r *http.Request, owner string) {
	filter := r.URL.Query().Get("filter")
	key := r.Header.Get("Idempotency-Key")
	if !filters[filter] || len(key) > 128 {
		fail(w, 400, "invalid_filter_or_key")
		return
	}
	if r.ContentLength > maxImageBytes {
		fail(w, 413, "image_too_large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes)
	data, err := io.ReadAll(r.Body)
	var sizeErr *http.MaxBytesError
	if errors.As(err, &sizeErr) {
		fail(w, 413, "image_too_large")
		return
	}
	if err != nil {
		fail(w, 400, "invalid_body")
		return
	}
	switch http.DetectContentType(data) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		fail(w, 415, "unsupported_image")
		return
	}
	h := sha256.New()
	io.WriteString(h, filter+"\x00")
	h.Write(data)
	hash := hex.EncodeToString(h.Sum(nil))
	if key != "" && s.replay(w, r, owner, key, hash) {
		return
	}
	id := newID()
	source := "source/" + id
	response, err := s.processorRequest(r.Context(), "PUT", "/objects/"+source, bytes.NewReader(data))
	if err != nil {
		fail(w, 503, "storage_unavailable")
		return
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		fail(w, 503, "storage_unavailable")
		return
	}
	created, err := s.createJob(r.Context(), id, owner, filter, source, hash, key)
	if err != nil {
		fail(w, 503, "database_unavailable")
		return
	}
	if !created {
		s.cleanupObject(r.Context(), source)
		if !s.replay(w, r, owner, key, hash) {
			fail(w, 503, "database_unavailable")
		}
		return
	}
	slog.Info("job accepted", "task_id", id)
	accepted(w, id)
}

func (s *service) replay(w http.ResponseWriter, r *http.Request, owner, key, hash string) bool {
	id, previous, err := s.byKey(r.Context(), owner, key)
	switch {
	case missing(err):
		return false
	case err != nil:
		fail(w, 503, "database_unavailable")
	case previous != hash:
		fail(w, 409, "idempotency_conflict")
	default:
		accepted(w, id)
	}
	return true
}

func accepted(w http.ResponseWriter, id string) {
	w.Header().Set("Location", "/status/"+id)
	respond(w, 202, map[string]string{"task_id": id})
}

func (s *service) ownedJob(w http.ResponseWriter, r *http.Request, owner string) (job, bool) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		fail(w, 404, "task_not_found")
		return job{}, false
	}
	j, err := s.findJob(r.Context(), owner, id)
	if missing(err) {
		fail(w, 404, "task_not_found")
		return job{}, false
	}
	if err != nil {
		fail(w, 503, "database_unavailable")
		return job{}, false
	}
	return j, true
}

func (s *service) status(w http.ResponseWriter, r *http.Request, owner string) {
	if j, ok := s.ownedJob(w, r, owner); ok {
		respond(w, 200, j)
	}
}

func (s *service) result(w http.ResponseWriter, r *http.Request, owner string) {
	j, ok := s.ownedJob(w, r, owner)
	if !ok {
		return
	}
	if j.result == nil {
		fail(w, 409, "result_unavailable")
		return
	}
	response, err := s.processorRequest(r.Context(), "GET", "/objects/"+*j.result, nil)
	if err != nil {
		fail(w, 503, "storage_unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		fail(w, 503, "storage_unavailable")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, no-store")
	if length := response.Header.Get("Content-Length"); length != "" {
		w.Header().Set("Content-Length", length)
	}
	if _, err := io.Copy(w, response.Body); err != nil {
		slog.Warn("result transfer interrupted", "task_id", j.ID, "error", err)
	}
}

func (s *service) processorRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.processor, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	return s.client.Do(request)
}

func (s *service) deleteObject(ctx context.Context, key string) error {
	response, err := s.processorRequest(ctx, "DELETE", "/objects/"+key, nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		return fmt.Errorf("delete object: status %d", response.StatusCode)
	}
	return nil
}

func (s *service) cleanupObject(ctx context.Context, key string) {
	if err := s.deleteObject(ctx, key); err != nil {
		slog.Warn("object cleanup failed", "key", key, "error", err)
	}
}
