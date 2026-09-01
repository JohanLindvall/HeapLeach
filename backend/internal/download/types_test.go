package download

import "testing"

// A job's wire view counts its items once and derives the status from that
// count; the status a job reports on its own walks the items itself. The two
// have to agree, whatever the mix, or the sidebar's filter and the card's
// badge would disagree about the same job.
func TestJobViewAgreesWithStatus(t *testing.T) {
	tests := []struct {
		name     string
		job      Job
		statuses []Status
		want     Status
	}{
		{"resolving outranks everything", Job{resolving: true}, []Status{StatusRunning}, StatusResolving},
		{"cancelled outranks the items", Job{canceled: true}, []Status{StatusDone, StatusDone}, StatusCanceled},
		{"an extractor failure with nothing found", Job{Err: "no files"}, nil, StatusFailed},
		{"nothing resolved yet", Job{}, nil, StatusQueued},
		{"anything running", Job{}, []Status{StatusDone, StatusRunning, StatusQueued}, StatusRunning},
		{"anything still waiting", Job{}, []Status{StatusDone, StatusQueued}, StatusQueued},
		{"a failure among successes", Job{}, []Status{StatusDone, StatusFailed, StatusCanceled}, StatusFailed},
		{"all done", Job{}, []Status{StatusDone, StatusDone}, StatusDone},
		{"done and cancelled is done", Job{}, []Status{StatusDone, StatusCanceled}, StatusDone},
		{"every item cancelled", Job{}, []Status{StatusCanceled, StatusCanceled}, StatusCanceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := tc.job
			for i, s := range tc.statuses {
				job.Items = append(job.Items, &Item{ID: string(rune('a' + i)), Status: s})
			}
			view := job.view()
			if view.Status != tc.want {
				t.Errorf("view status = %s, want %s", view.Status, tc.want)
			}
			if got := job.status(); got != view.Status {
				t.Errorf("status() = %s but the view says %s", got, view.Status)
			}
			tally := job.tally()
			if view.Done != tally.done || view.Failed != tally.failed ||
				view.Canceled != tally.canceled || view.Active != tally.running {
				t.Errorf("view counts %d/%d/%d/%d, tally %+v",
					view.Done, view.Failed, view.Canceled, view.Active, tally)
			}
			if view.Total != len(tc.statuses) {
				t.Errorf("Total = %d, want %d", view.Total, len(tc.statuses))
			}
		})
	}
}
