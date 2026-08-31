package artifactory

import "sync"

// batchQueue hands work to the scanner's workers, and takes retries back.
//
// # Why a queue rather than a fixed partition
//
// Because the batch size is not fixed any more. The old shape decided every
// batch up front - `batchIndexes(queryable, 50)` - which meant the pacer's
// answer to "fifty is too many for this Xray right now" arrived after every
// batch had already been cut at fifty. A worker that asks for its next batch
// when it is ready to send one gets the size the scanner has just proved it can
// answer.
//
// # Why retries come back here instead of recursing
//
// A batch that times out is split, and the halves used to be re-sent by the
// goroutine that failed - one after the other, inside the slot it already held.
// So a batch needing four splits paid four sixty-second timeouts in series
// while nine other workers sat idle, and the concurrency limit that was
// supposed to bound the load instead bounded the recovery.
//
// Pushed back onto the queue, the halves are picked up by whichever worker is
// free, in parallel, still inside the same global allowance. The load on the
// scanner is unchanged - the semaphore decides that, not the call graph - and
// the wall clock is not.
//
// Retries are taken BEFORE fresh work. A split that waits behind two hundred
// unsent artifacts is a split that lands after the sync has given up, and the
// artifacts it covers are the ones already known to be difficult.
type batchQueue struct {
	mu   sync.Mutex
	cond *sync.Cond

	// retries are split batches waiting to be re-sent, newest first.
	retries [][]int
	// rest is everything not yet dispatched, in order.
	rest []int
	// active is how many batches are in flight. It is what makes "the queue is
	// empty" different from "the work is finished": an in-flight batch may yet
	// push two retries.
	active int
	closed bool
}

func newBatchQueue(indexes []int) *batchQueue {
	q := &batchQueue{rest: indexes}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Take returns the next batch of at most `size`, or nil when there is nothing
// left and nothing in flight that could produce more.
//
// Blocks rather than spinning while other workers are busy, because a worker
// that returned nil early would exit and leave the retries those workers are
// about to push with nobody to run them.
func (q *batchQueue) Take(size int) []int {
	if size <= 0 {
		size = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	for {
		if n := len(q.retries); n > 0 {
			batch := q.retries[n-1]
			q.retries = q.retries[:n-1]
			q.active++
			return batch
		}
		if len(q.rest) > 0 {
			if size > len(q.rest) {
				size = len(q.rest)
			}
			batch := q.rest[:size]
			q.rest = q.rest[size:]
			q.active++
			return batch
		}
		if q.active == 0 || q.closed {
			// Everybody else is idle too, so nothing more can arrive. Wake the
			// rest so they reach this line rather than waiting for a signal
			// that will never come.
			q.cond.Broadcast()
			return nil
		}
		q.cond.Wait()
	}
}

// Done returns a batch's slot and queues whatever it wants retried.
func (q *batchQueue) Done(retries ...[]int) {
	q.mu.Lock()
	q.active--
	for _, r := range retries {
		if len(r) > 0 {
			q.retries = append(q.retries, r)
		}
	}
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Close stops the queue handing out work, for a cancelled sync.
//
// Waiting workers are woken so they exit rather than blocking on a Cond nobody
// will signal - which on a cancelled context is a goroutine leak per worker,
// held for as long as the process runs.
func (q *batchQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}
