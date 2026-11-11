package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

func (s *service) collectOrphans(ctx context.Context, remove bool) error {
	cursor := ""
	for {
		response, err := s.processorRequest(ctx, "GET", "/objects?cursor="+url.QueryEscape(cursor), nil)
		if err != nil {
			return err
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			return fmt.Errorf("list objects: status %d", response.StatusCode)
		}
		var page struct {
			Keys   []string `json:"keys"`
			Cursor string   `json:"cursor"`
		}
		err = json.NewDecoder(response.Body).Decode(&page)
		response.Body.Close()
		if err != nil {
			return err
		}
		for _, key := range page.Keys {
			parts := strings.Split(key, "/")
			if len(parts) < 2 || !uuidPattern.MatchString(parts[1]) {
				continue
			}
			var retained bool
			err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_jobs
   WHERE id=$1::text::uuid AND ($2=source_key OR $2=result_key
    OR (status='processing' AND $2='result/'||id::text||'/'||lease_token::text)))`, parts[1], key).Scan(&retained)
			if err != nil {
				return err
			}
			if retained {
				continue
			}
			slog.Info("orphan object found", "key", key, "deleting", remove)
			if remove {
				if err := s.deleteObject(ctx, key); err != nil {
					return err
				}
			}
		}
		if page.Cursor == "" {
			return nil
		}
		cursor = page.Cursor
	}
}
