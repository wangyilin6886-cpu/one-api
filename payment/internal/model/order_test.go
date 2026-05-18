package model

import "testing"

func TestOrder_StateMachine(t *testing.T) {
	type tc struct {
		from, to OrderStatus
		want     bool // true = allowed
	}
	cases := []tc{
		// pending out-edges
		{StatusPending, StatusPaid, true},
		{StatusPending, StatusExpired, true},
		{StatusPending, StatusCanceled, true},
		{StatusPending, StatusFailed, true},
		{StatusPending, StatusNeedsManualReview, true},
		{StatusPending, StatusCredited, false},
		{StatusPending, StatusRefunding, false},
		{StatusPending, StatusRefunded, false},

		// paid out-edges
		{StatusPaid, StatusCredited, true},
		{StatusPaid, StatusNeedsManualReview, true},
		{StatusPaid, StatusExpired, false},
		{StatusPaid, StatusPending, false},

		// credited out-edges (PR #3 territory but the table allows it)
		{StatusCredited, StatusRefunding, true},
		{StatusCredited, StatusPending, false},
		{StatusCredited, StatusPaid, false},

		// late callback escalation
		{StatusExpired, StatusNeedsManualReview, true},
		{StatusCanceled, StatusNeedsManualReview, true},
		{StatusFailed, StatusNeedsManualReview, true},

		// terminal-terminal blocked
		{StatusRefunded, StatusCredited, false},
		{StatusRefunded, StatusRefunding, false},
	}
	for _, c := range cases {
		o := &Order{Status: c.from}
		err := o.CanTransition(c.to)
		got := err == nil
		if got != c.want {
			t.Errorf("%s -> %s: want %v, got %v (err=%v)", c.from, c.to, c.want, got, err)
		}
	}
}

func TestOrder_IsPostPayTerminal(t *testing.T) {
	// Corrections 1 & 2: these statuses MUST NOT auto-credit on late webhook.
	wantTrue := []OrderStatus{
		StatusExpired, StatusCanceled, StatusFailed,
		StatusNeedsManualReview, StatusRefunding, StatusRefunded,
	}
	// These are still in the "live" lifecycle and CAN process a late paid webhook.
	wantFalse := []OrderStatus{
		StatusPending, StatusPaid, StatusCredited,
	}
	for _, s := range wantTrue {
		o := &Order{Status: s}
		if !o.IsPostPayTerminal() {
			t.Errorf("status %s: want IsPostPayTerminal=true, got false", s)
		}
	}
	for _, s := range wantFalse {
		o := &Order{Status: s}
		if o.IsPostPayTerminal() {
			t.Errorf("status %s: want IsPostPayTerminal=false, got true", s)
		}
	}
}
