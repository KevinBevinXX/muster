package agents

import "regexp"

// workerPoolPattern is what a worker pool's name may be. Migration 0041's
// agents_worker_pool_check and jobs_worker_pool_check spell the same
// expression.
var workerPoolPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidWorkerPool reports whether name may name a worker pool: 1-63 of
// [a-z0-9-], not starting with '-'.
func ValidWorkerPool(name string) bool { return workerPoolPattern.MatchString(name) }
