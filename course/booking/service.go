package booking

import (
	"context"
	"errors"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"time"

	"github.com/imrenagicom/demo-app/course/catalog"
	"github.com/imrenagicom/demo-app/internal/db"
	v1 "github.com/imrenagicom/demo-app/pkg/apiclient/course/v1"
	"github.com/jmoiron/sqlx"
	"go.opentelemetry.io/otel"
)

var tracer = otel.Tracer("github.com/fauzanalghifary/demo-app/course/booking")
var meter = otel.Meter("github.com/fauzanalghifary/demo-app/course/booking")

const (
	maxReservationAttemptRetry = 5
	maxReleaseAttemptRetry     = 5
)

func NewService(
	db *sqlx.DB,
	bookingStore *Store,
	catalogStore *catalog.Store,
) *Service {
	bookingReservedCtr, err := meter.Int64Counter(
		"booking.reserved",
		metric.WithDescription("number of reserved bookings"),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create metric")
	}

	bookingExpiredCtr, err := meter.Int64Counter(
		"booking.expired",
		metric.WithDescription("number of expired bookings"),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create metric")
	}

	bookingReservedAmtCtr, err := meter.Float64Counter(
		"booking.reserved.amount",
		metric.WithDescription("total amount of reserved bookings"),
		metric.WithUnit("IDR"),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create metric")
	}

	return &Service{
		db:           db,
		bookingStore: bookingStore,
		catalogStore: catalogStore,

		bookingReservedCtr:    bookingReservedCtr,
		bookingExpiredCtr:     bookingExpiredCtr,
		bookingReservedAmtCtr: bookingReservedAmtCtr,
	}
}

type Service struct {
	db           *sqlx.DB
	bookingStore *Store
	catalogStore *catalog.Store

	bookingReservedCtr    metric.Int64Counter
	bookingExpiredCtr     metric.Int64Counter
	bookingReservedAmtCtr metric.Float64Counter
}

// CreateBooking creates a new booking for the given course and batch and emits BookingCreated event.
func (s Service) CreateBooking(ctx context.Context, req *v1.CreateBookingRequest) (
	*Booking,
	error,
) {
	course, err := s.catalogStore.FindCourseByID(ctx, req.Booking.GetCourse())
	if err != nil {
		return nil, err
	}

	batch, err := s.catalogStore.FindCourseBatchByID(ctx, req.Booking.GetBatch())
	if err != nil {
		return nil, err
	}

	if err = batch.Available(ctx); err != nil {
		return nil, err
	}

	builder := For(course, batch)
	if req.Booking.Customer != nil {
		// TODO validate customer data
		c := req.Booking.Customer
		builder.WithCustomer(c.Name, c.Email, c.PhoneNumber)
	}
	b := builder.Build()

	err = s.bookingStore.CreateBooking(ctx, b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s Service) ReserveBooking(ctx context.Context, req *v1.ReserveBookingRequest) (
	*Booking,
	error,
) {
	ctx, span := tracer.Start(ctx, "ReserveBooking")
	defer span.End()
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}

	booking, err := s.bookingStore.FindBookingByID(ctx, req.GetBooking(), WithFindTx(tx))
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	if err = s.reserveWithRetry(ctx, tx, booking, 0); err != nil {
		tx.Rollback()
		return nil, err
	}

	if err = s.bookingStore.UpdateBookingStatus(ctx, booking, WithUpdateTx(tx)); err != nil {
		tx.Rollback()
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		tx.Rollback()
		return nil, err
	}

	log.Info().
		Float64("price", booking.Price).
		Msgf("Booking %s reserved", booking.ID.String())

	s.bookingReservedCtr.Add(ctx, 1)
	s.bookingReservedAmtCtr.Add(ctx, booking.Price)

	return booking, nil
}

func (s Service) reserveWithRetry(
	ctx context.Context,
	tx *sqlx.Tx,
	b *Booking,
	retryCount int,
) error {
	ctx, span := tracer.Start(
		ctx, "reserveWithRetry", trace.WithAttributes(
			attribute.Int("retryCount", retryCount),
		),
	)
	defer span.End()
	if retryCount > maxReservationAttemptRetry {
		return ErrReservationMaxRetryExceeded
	}

	tc, err := s.catalogStore.FindCourseBatchByIDAndCourseID(
		ctx,
		b.Batch.ID.String(),
		b.Course.ID.String(),
		catalog.WithFindTx(tx),
	)
	if err != nil {
		return err
	}

	if err := b.Reserve(ctx, tc); err != nil {
		return err
	}

	err = s.catalogStore.UpdateBatchAvailableSeats(ctx, tc, catalog.WithUpdateTx(tx))
	if err != nil && !errors.Is(err, db.ErrNoRowUpdated) {
		return err
	}
	if errors.Is(err, db.ErrNoRowUpdated) {
		return s.reserveWithRetry(ctx, tx, b, retryCount+1)
	}
	return nil
}

func (s Service) GetBooking(ctx context.Context, req *v1.GetBookingRequest) (*Booking, error) {
	return s.bookingStore.FindBookingByID(ctx, req.GetBooking())
}

func (s Service) ExpireBooking(ctx context.Context, req *v1.ExpireBookingRequest) error {
	ctx, span := tracer.Start(ctx, "ExpireBooking")
	defer span.End()

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}

	b, err := s.bookingStore.FindBookingByID(
		ctx,
		req.GetBooking(),
		WithDisableCache(),
		WithFindTx(tx),
	)
	if err != nil {
		tx.Rollback()
		return err
	}

	if err = b.Expire(ctx); err != nil {
		tx.Rollback()
		return err
	}

	ctx, _ = context.WithTimeout(ctx, 5*time.Millisecond)
	if err = s.bookingStore.UpdateBookingStatus(ctx, b, WithUpdateTx(tx)); err != nil {
		tx.Rollback()
		return err
	}

	if err = s.releaseBooking(ctx, tx, b, 0); err != nil {
		tx.Rollback()
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	s.bookingExpiredCtr.Add(ctx, 1)

	return nil
}

func (s Service) releaseBooking(
	ctx context.Context,
	tx *sqlx.Tx,
	b *Booking,
	retryCount int,
) error {
	ctx, span := tracer.Start(
		ctx, "releaseBooking", trace.WithAttributes(
			attribute.Int("retryCount", retryCount),
		),
	)
	defer span.End()

	if retryCount > maxReleaseAttemptRetry {
		return ErrReleaseMaxRetryExceeded
	}

	batch, err := s.catalogStore.FindCourseBatchByIDAndCourseID(
		ctx,
		b.Batch.ID.String(),
		b.Course.ID.String(),
		catalog.WithFindTx(tx),
	)
	if err != nil {
		return err
	}

	err = batch.Allocate(ctx, 1)
	if err != nil {
		return err
	}

	err = s.catalogStore.UpdateBatchAvailableSeats(ctx, batch, catalog.WithUpdateTx(tx))
	if err != nil && !errors.Is(err, db.ErrNoRowUpdated) {
		return err
	}
	if errors.Is(err, db.ErrNoRowUpdated) {
		return s.releaseBooking(ctx, tx, b, retryCount+1)
	}
	return nil
}

func (s Service) ListBookings(ctx context.Context, req *v1.ListBookingsRequest) (
	[]Booking,
	string,
	error,
) {
	return s.bookingStore.FindAllBookings(ctx, WithFindAllInvoiceNumber(req.GetInvoice()))
}
