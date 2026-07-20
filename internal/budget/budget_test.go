package budget

import (
	"context"
	"errors"
	"testing"
)

type recordingRepository struct {
	taskID          string
	estimatedTokens int
	limits          Limits
	reservationID   string
	actualTokens    int
	confirmed       bool
	reserveErr      error
	settleErr       error
}

func (r *recordingRepository) ReserveModelTokens(_ context.Context, taskID string, estimatedTokens int, limits Limits) (string, error) {
	r.taskID = taskID
	r.estimatedTokens = estimatedTokens
	r.limits = limits
	return "reservation", r.reserveErr
}

func (r *recordingRepository) SettleModelTokens(_ context.Context, reservationID string, actualTokens int, confirmed bool) error {
	r.reservationID = reservationID
	r.actualTokens = actualTokens
	r.confirmed = confirmed
	return r.settleErr
}

func TestServiceValidatesLimitsAndUsage(t *testing.T) {
	if _, err := NewService(nil, Limits{PerTask: 1, PerDay: 1, PerMonth: 1}); err == nil {
		t.Fatal("nil repository was accepted")
	}
	for _, limits := range []Limits{
		{},
		{PerTask: 2, PerDay: 1, PerMonth: 3},
		{PerTask: 1, PerDay: 3, PerMonth: 2},
	} {
		if _, err := NewService(&recordingRepository{}, limits); err == nil {
			t.Fatalf("invalid limits were accepted: %+v", limits)
		}
	}

	service, err := NewService(&recordingRepository{}, Limits{PerTask: 10, PerDay: 20, PerMonth: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reserve(context.Background(), "task", 0); err == nil {
		t.Fatal("zero-sized reservation was accepted")
	}
	if err := service.Settle(context.Background(), "", 1, true); err == nil {
		t.Fatal("empty reservation id was accepted")
	}
	if err := service.Settle(context.Background(), "reservation", -1, true); err == nil {
		t.Fatal("negative settlement was accepted")
	}
}

func TestServiceDelegatesExactReservationAndSettlement(t *testing.T) {
	repository := &recordingRepository{}
	limits := Limits{PerTask: 100, PerDay: 200, PerMonth: 300}
	service, err := NewService(repository, limits)
	if err != nil {
		t.Fatal(err)
	}

	id, err := service.Reserve(context.Background(), "task-1", 42)
	if err != nil || id != "reservation" {
		t.Fatalf("reserve id=%q err=%v", id, err)
	}
	if repository.taskID != "task-1" || repository.estimatedTokens != 42 || repository.limits != limits {
		t.Fatalf("reservation changed at boundary: %+v", repository)
	}

	if err := service.Settle(context.Background(), id, 37, false); err != nil {
		t.Fatal(err)
	}
	if repository.reservationID != id || repository.actualTokens != 37 || repository.confirmed {
		t.Fatalf("settlement changed at boundary: %+v", repository)
	}
}

func TestServicePreservesRepositoryFailures(t *testing.T) {
	reserveErr := errors.New("reserve failed")
	settleErr := errors.New("settle failed")
	repository := &recordingRepository{reserveErr: reserveErr, settleErr: settleErr}
	service, err := NewService(repository, Limits{PerTask: 10, PerDay: 20, PerMonth: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reserve(context.Background(), "task", 1); !errors.Is(err, reserveErr) {
		t.Fatalf("reserve error = %v", err)
	}
	if err := service.Settle(context.Background(), "reservation", 1, true); !errors.Is(err, settleErr) {
		t.Fatalf("settle error = %v", err)
	}
}
