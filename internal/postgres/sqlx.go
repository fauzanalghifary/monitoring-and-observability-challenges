package postgres

import (
	"github.com/imrenagicom/demo-app/internal/config"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"

	"github.com/jmoiron/sqlx"
	"github.com/uptrace/opentelemetry-go-extra/otelsql"
	"github.com/uptrace/opentelemetry-go-extra/otelsqlx"
)

func NewSQLx(c config.SQL) *sqlx.DB {
	db, err := otelsqlx.Open(
		"postgres",
		c.DataSourceName(),
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
	)
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(c.MaxOpenConn)
	db.SetMaxIdleConns(c.MaxIdleConn)
	return db
}
