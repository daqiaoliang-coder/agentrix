package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/daqiaoliang-coder/agentrix/internal/harness/core"
	"github.com/daqiaoliang-coder/agentrix/internal/session"
)

func TestStatusOfMapsWrappedTurnErrors(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{errSceneNotFound, http.StatusNotFound},
		{errResumeParams, http.StatusBadRequest},
		{core.ErrSessionBusy, http.StatusConflict},
		{core.ErrSessionLeaseLost, http.StatusConflict},
		{core.ErrResumeInProgress, http.StatusConflict},
		{core.ErrInterruptMismatch, http.StatusConflict},
		{session.ErrCASConflict, http.StatusConflict},
		{core.ErrNoPendingApproval, http.StatusGone},
		{core.ErrCheckpointNotFound, http.StatusGone},
		{fmt.Errorf("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		wrapped := fmt.Errorf("turn failed: %w", c.err)
		if got := statusOf(wrapped); got != c.want {
			t.Errorf("statusOf(%v) = %d, want %d", wrapped, got, c.want)
		}
	}
}
