package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/Masterminds/squirrel"
	cdb "github.com/contiamo/go-base/v2/pkg/db"
	"github.com/contiamo/go-base/v2/pkg/queue"
	"github.com/contiamo/go-base/v2/pkg/queue/handlers"
	"github.com/opentracing/opentracing-go"
)

const (
	// MaintenanceTaskQueue task queue name used for all the periodic maintenance jobs.
	// These are internal queue internal tasks
	MaintenanceTaskQueue string = "queue-maintenance"

	// RetentionTask is finished task cleanup type
	RetentionTask queue.TaskType = "retention"
)

// retentionTaskSpec defines a SQL task to remove completed tasks that match given criteria.
type retentionTaskSpec struct {
	handlers.SQLExecTaskSpec
	QueueName string           `json:"queueName"`
	TaskType  queue.TaskType   `json:"taskType"`
	Status    queue.TaskStatus `json:"status"`
	Age       time.Duration    `json:"age"`
}

// NewRetentionHandler creates a task handler that will clean up old finished tasks
func NewRetentionHandler(db *sql.DB) queue.TaskHandler {
	return handlers.NewSQLTaskHandler("RetentionHandler", db)
}

// AssertRetentionSchedule creates a new queue retention tasks for the supplied queue, finished tasks matching
// the supplied parameters will be deleted
func AssertRetentionSchedule(ctx context.Context, db *sql.DB, queueName string, taskType queue.TaskType, status queue.TaskStatus, age time.Duration) (err error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "AssertRetentionSchedule")
	span.SetTag("pkg.name", "postgres")

	spec := createRetentionSpec(queueName, taskType, status, age)
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("can not build retention task spec: %w", err)
	}
	// randomly distribute the retention tasks throughout the hour
	when := rand.Intn(60)
	retentionSchedule := queue.TaskScheduleRequest{
		TaskBase: queue.TaskBase{
			Queue: MaintenanceTaskQueue,
			Type:  RetentionTask,
			Spec:  specBytes,
		},
		CronSchedule: fmt.Sprintf("%d * * * *", when), // every hour at minute "when"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("can not start transaction for scheduling: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()

	_, err = tx.ExecContext(ctx, `LOCK TABLE schedules IN ACCESS EXCLUSIVE MODE;`)
	if err != nil {
		return fmt.Errorf("failed to lock `schedules`: %w", err)
	}

	builder := squirrel.StatementBuilder.
		PlaceholderFormat(squirrel.Dollar).
		RunWith(cdb.WrapWithTracing(tx))

	var exists int
	// use a unique error name here otherwise the sql.ErrNoRows might shadow
	// us and things will break. This is also handled by the named error return
	// variable, but this makes the code easier to copy and paste
	existsErr := builder.Select("1").
		From("schedules").
		Where(squirrel.Eq{
			"task_queue":              MaintenanceTaskQueue,
			"task_type":               RetentionTask,
			"task_spec->>'queueName'": queueName,
			"task_spec->>'taskType'":  taskType,
			"task_spec->>'status'":    status,
		}).ScanContext(ctx, &exists)
	if existsErr != nil && existsErr != sql.ErrNoRows {
		return fmt.Errorf("can not verify existing schedule: %w", existsErr)
	}

	// will only non-zero if err is nil and task is not found
	if exists == 0 {
		span.SetTag("created", true)
		// pass nil db because it doesn't need the raw db
		return NewScheduler(nil).Schedule(ctx, builder, retentionSchedule)
	}

	span.SetTag("updated", true)
	res, err := builder.Update("schedules").
		Where(squirrel.Eq{
			"task_queue":              MaintenanceTaskQueue,
			"task_type":               RetentionTask,
			"task_spec->>'queueName'": queueName,
			"task_spec->>'taskType'":  taskType,
			"task_spec->>'status'":    status,
		}).
		Set("updated_at", time.Now()).
		Set("task_spec", retentionSchedule.Spec).
		Set("cron_schedule", retentionSchedule.CronSchedule).
		Set("next_execution_time", time.Now()).
		ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("can not update existing schdule: %w", err)
	}

	updated, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("can not determine the number of rows affected: %w", err)
	}

	span.SetTag("affected", updated)

	return nil
}

//createRetentionSpec builds the task retention job spec. It is split out to simplify test setup
func createRetentionSpec(queueName string, taskType queue.TaskType, status queue.TaskStatus, age time.Duration) retentionTaskSpec {
	spec := retentionTaskSpec{
		QueueName: queueName,
		TaskType:  taskType,
		Status:    status,
		Age:       age,
	}

	// use separate WHERE statements to make the order deterministic
	deletionSQL := squirrel.Delete(TasksTable).
		Where(squirrel.Eq{"status": status}).
		Where(
			// note that using this comparision allows us to use the index on
			// finished_at, if yo use `age(now(), finished_at)`, this can not use the index
			fmt.Sprintf("finished_at <= now() - interval '%f minutes'", age.Minutes()),
		)

	if queueName != "" {
		deletionSQL = deletionSQL.Where(squirrel.Eq{"queue": queueName})
	}

	if taskType != "" {
		deletionSQL = deletionSQL.Where(squirrel.Eq{"type": taskType})
	}

	spec.SQL = squirrel.DebugSqlizer(deletionSQL)

	return spec
}
