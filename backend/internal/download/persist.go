package download

import (
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Restore reads back the queue a previous run left behind, and reports how
// many jobs still have work in them.
//
// Call before Start. Nothing is fetched here and nothing is queued: an
// unfinished job comes back held, with the items it had last time on show
// and a note to itself that the source must be read again before any of them
// can be transferred. Resuming is the user's word to give — Resume, or a
// retry on the one job they care about.
//
// A file that cannot be read is reported and otherwise ignored. The queue is
// a convenience; refusing to start over a corrupt one would turn a lost list
// into a lost service.
func (m *Manager) Restore() (unfinished int, err error) {
	if m.stateFile == "" {
		return 0, nil
	}

	st, err := loadState(m.stateFile)
	if err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, sj := range st.Jobs {
		job := &Job{
			ID:        sj.ID,
			Source:    sj.Source,
			Title:     sj.Title,
			Host:      sj.Host,
			Password:  sj.Password,
			Err:       sj.Err,
			CreatedAt: sj.CreatedAt,
			canceled:  sj.Canceled,
		}
		for _, si := range sj.Items {
			it := &Item{
				ID:      si.ID,
				Name:    si.Name,
				Dir:     si.Dir,
				Path:    si.Path,
				Status:  si.Status,
				Size:    si.Size,
				Err:     si.Err,
				Skipped: si.Skipped,
			}
			// A finished file keeps its byte count so the job still reads as
			// complete. An unfinished one starts from zero on purpose: what
			// is really on disk is whatever its part file holds, which the
			// worker measures rather than trusts a number for.
			if it.Status == StatusDone {
				it.downloaded.Store(si.Size)
			}
			job.Items = append(job.Items, it)
		}

		if jobHasWorkLeft(job) {
			job.restored = true
			unfinished++
			// Without this the items read as "queued", which is what an item
			// about to start says — and these are waiting on a person, not on
			// a worker. Note is exactly the field for a wait that the numbers
			// cannot explain, and the UI already shows it.
			for _, it := range job.Items {
				if it.Status != StatusDone && it.Status != StatusCanceled {
					it.Note = "held since the last run — retry this job to pick it up"
				}
			}
		}
		m.jobs[job.ID] = job
		m.order = append(m.order, job.ID)
	}

	m.markDirty()
	return unfinished, nil
}

// jobHasWorkLeft reports whether anything about a job is worth doing again.
//
// A job that resolved to nothing counts: it never got as far as producing an
// item, so there is no other trace that it was left undone.
func jobHasWorkLeft(job *Job) bool {
	if len(job.Items) == 0 {
		return !job.canceled
	}
	for _, it := range job.Items {
		if it.Status != StatusDone && it.Status != StatusCanceled {
			return true
		}
	}
	return false
}

// stateLocked renders the queue for the state file. Caller holds mu.
func (m *Manager) stateLocked() *savedState {
	st := &savedState{Version: stateVersion, Jobs: make([]savedJob, 0, len(m.order))}
	for _, id := range m.order {
		job, ok := m.jobs[id]
		if !ok {
			continue
		}
		sj := savedJob{
			ID:        job.ID,
			Source:    job.Source,
			Title:     job.Title,
			Host:      job.Host,
			Password:  job.Password,
			Err:       job.Err,
			CreatedAt: job.CreatedAt,
			Canceled:  job.canceled,
			Items:     make([]savedItem, 0, len(job.Items)),
		}
		for _, it := range job.Items {
			status := it.Status
			// Anything in flight is written down as queued. A transfer the
			// process did not live to finish had not finished, and recording
			// it as running would restore a state nothing is in.
			if status == StatusRunning || status == StatusResolving {
				status = StatusQueued
			}
			sj.Items = append(sj.Items, savedItem{
				ID:      it.ID,
				Name:    it.Name,
				Dir:     it.Dir,
				Path:    it.Path,
				Status:  status,
				Size:    it.Size,
				Err:     it.Err,
				Skipped: it.Skipped,
			})
		}
		st.Jobs = append(st.Jobs, sj)
	}
	return st
}

// persist writes the queue when it has changed since the last write.
//
// Building the record needs mu; writing it does not, and must not — the
// locking rule here is that mu is never held across anything that blocks,
// and this ends in an fsync. So the record is built under the lock, and the
// file is written after it has been let go.
func (m *Manager) persist() {
	if m.stateFile == "" {
		return
	}

	m.mu.Lock()
	st := m.stateLocked()
	m.mu.Unlock()

	// The byte counters move constantly and are never written down, so a
	// queue that is merely transferring fingerprints the same as it did a
	// moment ago and costs nothing beyond building the record.
	if print := st.fingerprint(); print == m.statePrint {
		return
	} else {
		m.statePrint = print
	}

	st.Saved = time.Now()
	if err := saveState(m.stateFile, st); err != nil {
		m.log.Warn("could not write the queue", "file", m.stateFile, "err", err)
		// Forget the fingerprint so the next interval tries again rather
		// than concluding the file is already current.
		m.statePrint = 0
	}
}

// saver writes the queue on its own slow schedule, and once more on the way
// out so a clean shutdown never loses the last minute of it.
func (m *Manager) saver() {
	defer m.wg.Done()

	ticker := time.NewTicker(config.StateSaveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			// Not written here: Close does it once every worker has stopped,
			// which is a later and truer picture than this goroutine racing
			// the others to describe one.
			return
		case <-ticker.C:
			m.persist()
		}
	}
}
