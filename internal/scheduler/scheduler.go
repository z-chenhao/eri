// Package scheduler owns durable future commitments and their deterministic
// time triggers. A trigger creates an ordinary Task; it does not contain a
// second cognitive loop.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/observability"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
)

type Schedule struct {
	Type            string    `json:"type"`
	At              time.Time `json:"at,omitempty"`
	AfterSeconds    int64     `json:"after_seconds,omitempty"`
	IntervalSeconds int64     `json:"interval_seconds,omitempty"`
	DailyTime       string    `json:"daily_time,omitempty"`
	Timezone        string    `json:"timezone,omitempty"`
}

type CreateRequest struct {
	Message       string   `json:"message"`
	Schedule      Schedule `json:"schedule"`
	Importance    string   `json:"importance,omitempty"`
	DeliveryRoute string   `json:"delivery_route,omitempty"`
}

const (
	DeliveryRouteOrigin = "origin_channel"
	DeliveryRouteRecent = "recent_channel"
)

// DeliveryTarget is a Runtime-owned routing fact. The model may request how a
// commitment follows the relationship, but trusted interaction records supply
// the actual Channel and any remote conversation identifiers.
type DeliveryTarget struct {
	Channel          string `json:"-"`
	ConversationID   string `json:"-"`
	ReplyToMessageID string `json:"-"`
	RoutingMode      string `json:"-"`
}

type Commitment struct {
	ID         string         `json:"id"`
	MessageRef content.Ref    `json:"-"`
	Schedule   Schedule       `json:"schedule"`
	Importance string         `json:"importance"`
	Target     DeliveryTarget `json:"-"`
	Status     string         `json:"status"`
	NextRunAt  time.Time      `json:"next_run_at"`
	LastRunAt  time.Time      `json:"last_run_at,omitempty"`
	Version    int            `json:"version"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

type Repository interface {
	CommitmentDeliveryTarget(context.Context, string) (DeliveryTarget, error)
	CreateCommitment(context.Context, Commitment) error
	UpdateCommitment(context.Context, Commitment) (Commitment, error)
	ListCommitments(context.Context, int) ([]Commitment, error)
	SetCommitmentStatus(context.Context, string, string) error
	TriggerDueCommitments(context.Context, time.Time, int) (int, error)
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Delete(context.Context, content.Ref) error
}

type Service struct {
	repository Repository
	content    ContentStore
	now        func() time.Time
}

func NewService(repository Repository, contentStore ContentStore) *Service {
	return &Service{repository: repository, content: contentStore, now: time.Now}
}

func (s *Service) Create(ctx context.Context, sourceTaskID string, request CreateRequest) (Commitment, error) {
	request, err := normalizeRequest(sourceTaskID, request)
	if err != nil {
		return Commitment{}, err
	}
	now := s.now().UTC()
	request.Schedule, err = resolveSchedule(request.Schedule, now)
	if err != nil {
		return Commitment{}, err
	}
	next, err := FirstRun(request.Schedule, now)
	if err != nil {
		return Commitment{}, err
	}
	target, err := s.deliveryTarget(ctx, sourceTaskID, request.DeliveryRoute)
	if err != nil {
		return Commitment{}, err
	}
	id, err := identifier.New()
	if err != nil {
		return Commitment{}, err
	}
	ref, err := s.storePrompt(ctx, id, request)
	if err != nil {
		return Commitment{}, err
	}
	commitment := Commitment{
		ID: id, MessageRef: ref, Schedule: request.Schedule, Importance: request.Importance,
		Target: target,
		Status: "active", NextRunAt: next, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repository.CreateCommitment(ctx, commitment); err != nil {
		_ = s.content.Delete(context.Background(), ref)
		return Commitment{}, err
	}
	return commitment, nil
}

func (s *Service) Update(ctx context.Context, sourceTaskID, id string, request CreateRequest) (Commitment, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Commitment{}, fmt.Errorf("commitment id is required")
	}
	if request.Importance == "" || request.DeliveryRoute == "" {
		return Commitment{}, fmt.Errorf("commitment update requires importance and delivery_route")
	}
	request, err := normalizeRequest(sourceTaskID, request)
	if err != nil {
		return Commitment{}, err
	}
	now := s.now().UTC()
	request.Schedule, err = resolveSchedule(request.Schedule, now)
	if err != nil {
		return Commitment{}, err
	}
	next, err := FirstRun(request.Schedule, now)
	if err != nil {
		return Commitment{}, err
	}
	ref, err := s.storePrompt(ctx, id, request)
	if err != nil {
		return Commitment{}, err
	}
	updated, err := s.repository.UpdateCommitment(ctx, Commitment{
		ID: id, MessageRef: ref, Schedule: request.Schedule, Importance: request.Importance,
		Target: DeliveryTarget{RoutingMode: request.DeliveryRoute}, NextRunAt: next, UpdatedAt: now,
	})
	if err != nil {
		_ = s.content.Delete(context.Background(), ref)
		return Commitment{}, err
	}
	return updated, nil
}

func resolveSchedule(schedule Schedule, now time.Time) (Schedule, error) {
	if schedule.AfterSeconds < 0 {
		return Schedule{}, fmt.Errorf("after_seconds must be positive")
	}
	if schedule.AfterSeconds == 0 {
		return schedule, nil
	}
	if schedule.Type != "once" {
		return Schedule{}, fmt.Errorf("after_seconds is supported only for one-time schedules")
	}
	if !schedule.At.IsZero() {
		return Schedule{}, fmt.Errorf("one-time schedule must use either at or after_seconds, not both")
	}
	schedule.At = now.Add(time.Duration(schedule.AfterSeconds) * time.Second).UTC()
	schedule.AfterSeconds = 0
	return schedule, nil
}

func normalizeRequest(sourceTaskID string, request CreateRequest) (CreateRequest, error) {
	sourceTaskID = strings.TrimSpace(sourceTaskID)
	if sourceTaskID == "" {
		return CreateRequest{}, fmt.Errorf("commitment source task id is required")
	}
	request.Message = strings.TrimSpace(request.Message)
	if request.Message == "" || len([]byte(request.Message)) > 64*1024 {
		return CreateRequest{}, fmt.Errorf("commitment message must be between 1 byte and 64 KiB")
	}
	if request.Importance == "" {
		request.Importance = "normal"
	}
	if request.Importance != "normal" && request.Importance != "important" {
		return CreateRequest{}, fmt.Errorf("importance must be normal or important")
	}
	if request.DeliveryRoute == "" {
		request.DeliveryRoute = DeliveryRouteOrigin
	}
	if request.DeliveryRoute != DeliveryRouteOrigin && request.DeliveryRoute != DeliveryRouteRecent {
		return CreateRequest{}, fmt.Errorf("delivery_route must be origin_channel or recent_channel")
	}
	return request, nil
}

func (s *Service) deliveryTarget(ctx context.Context, sourceTaskID, deliveryRoute string) (DeliveryTarget, error) {
	target, err := s.repository.CommitmentDeliveryTarget(ctx, sourceTaskID)
	if err != nil {
		return DeliveryTarget{}, fmt.Errorf("resolve commitment delivery target: %w", err)
	}
	target.RoutingMode = deliveryRoute
	return target, nil
}

func (s *Service) storePrompt(ctx context.Context, id string, request CreateRequest) (content.Ref, error) {
	prompt := "A durable commitment is due. Deliver this reminder or recurring task in the canonical conversation: " + request.Message
	if request.Importance == "important" {
		prompt += " Call the local notification tool once so the user can find this time-sensitive message even when the web page is closed."
	}
	return s.content.Put(ctx, []byte(prompt), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "commitment", PrivacyClass: "private",
		RetentionPolicy: "until_commitment_deleted", ProvenanceRef: id,
	})
}

func (s *Service) List(ctx context.Context, limit int) ([]Commitment, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.repository.ListCommitments(ctx, limit)
}

func (s *Service) SetStatus(ctx context.Context, id, status string) error {
	if status != "active" && status != "paused" && status != "canceled" {
		return fmt.Errorf("commitment status must be active, paused or canceled")
	}
	return s.repository.SetCommitmentStatus(ctx, id, status)
}

func FirstRun(schedule Schedule, now time.Time) (time.Time, error) {
	switch schedule.Type {
	case "once":
		if schedule.At.IsZero() || !schedule.At.After(now) {
			return time.Time{}, fmt.Errorf("one-time schedule must be in the future")
		}
		return schedule.At.UTC(), nil
	case "interval":
		if schedule.IntervalSeconds < 60 {
			return time.Time{}, fmt.Errorf("interval must be at least 60 seconds")
		}
		return now.Add(time.Duration(schedule.IntervalSeconds) * time.Second), nil
	case "daily":
		return nextDaily(schedule, now)
	default:
		return time.Time{}, fmt.Errorf("schedule type must be once, interval or daily")
	}
}

func NextRun(schedule Schedule, after time.Time) (time.Time, bool, error) {
	if schedule.Type == "once" {
		return time.Time{}, false, nil
	}
	if schedule.Type == "interval" {
		if schedule.IntervalSeconds < 60 {
			return time.Time{}, false, fmt.Errorf("invalid interval")
		}
		return after.Add(time.Duration(schedule.IntervalSeconds) * time.Second), true, nil
	}
	next, err := nextDaily(schedule, after)
	return next, err == nil, err
}

func nextDaily(schedule Schedule, after time.Time) (time.Time, error) {
	location := time.Local
	var err error
	if schedule.Timezone != "" {
		location, err = time.LoadLocation(schedule.Timezone)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid timezone")
		}
	}
	parsed, err := time.Parse("15:04", schedule.DailyTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("daily_time must use HH:MM")
	}
	local := after.In(location)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), parsed.Hour(), parsed.Minute(), 0, 0, location)
	if !candidate.After(local) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate.UTC(), nil
}

type Worker struct {
	repository Repository
	interval   time.Duration
	logger     *slog.Logger
}

func NewWorker(repository Repository, interval time.Duration, logger *slog.Logger) *Worker {
	if interval <= 0 {
		interval = time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{repository: repository, interval: interval, logger: logger}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if triggered, err := w.repository.TriggerDueCommitments(ctx, time.Now().UTC(), 20); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Error("scheduler trigger failed", "component", "scheduler", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		} else if triggered > 0 {
			w.logger.Info("scheduled commitments triggered", "component", "scheduler", "count", triggered)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
