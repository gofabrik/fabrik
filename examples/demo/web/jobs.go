package web

import (
	"context"
	"log/slog"

	"github.com/gofabrik/fabrik/query"
)

// Visit is the message enqueued to record a page visit.
type Visit struct {
	Path string `json:"path"`
}

//fabrik:job
func RecordVisit(ctx context.Context, q *query.DB, v Visit) error {
	type visit struct {
		Count int64 `db:"count"`
	}
	c, err := query.One[visit](ctx, q,
		`insert into visits (id, count) values (1, 1)
		 on conflict (id) do update set count = count + 1
		 returning count`)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "recorded visit", "path", v.Path, "count", c.Count)
	return nil
}

//fabrik:cron name=purge-greetings schedule="*/5 * * * *"
func PurgeGreetings(ctx context.Context, q *query.DB) error {
	res, err := q.ExecContext(ctx,
		`delete from greetings where id not in (select id from greetings order by id desc limit 5)`)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	slog.InfoContext(ctx, "purged greetings", "removed", n)
	return nil
}
