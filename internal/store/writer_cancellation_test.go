package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

type canceledBeginConnector struct {
	probeConnector
	cancel  context.CancelFunc
	failure error
}

func (c canceledBeginConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.probeConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return canceledBeginConn{Conn: conn, cancel: c.cancel, failure: c.failure}, nil
}

type canceledBeginConn struct {
	driver.Conn
	cancel  context.CancelFunc
	failure error
}

func (c canceledBeginConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.cancel()
	return nil, c.failure
}

func TestWriterCancellationDuringBeginPreservesCauseAndReleasesGate(t *testing.T) {
	t.Parallel()
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "driver-failure", true: "caller-canceled"}[canceled], func(t *testing.T) {
			s := testStore(t)
			original := s.writer
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("interrupted database call")
			trigger := func() {
				if canceled {
					cancel()
				}
			}
			db := sql.OpenDB(canceledBeginConnector{probeConnector: probeConnector{driver: original.Driver(), dsn: "file:" + s.path}, cancel: trigger, failure: failure})
			s.writer = db
			defer func() { s.writer = original; requireOK(t, db.Close()) }()
			tx, done, err := s.beginWrite(ctx)
			if tx != nil || done != nil || !errors.Is(err, failure) || errors.Is(err, context.Canceled) != canceled {
				t.Fatal("writer cancellation cause lost", err)
			}
			kind := unavailable
			if canceled {
				kind = locked
			}
			_, reason, remedy := message(kind)
			public := publicerr.Classify(err)
			if public.Code() != publicerr.Unavailable || public.Reason() != reason || public.Remedy() != remedy {
				t.Fatal("writer cancellation classification lost", err)
			}
			select {
			case s.writes <- struct{}{}:
				<-s.writes
			default:
				t.Fatal("failed begin kept the writer gate")
			}
		})
	}
}
