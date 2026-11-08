package sessions

import (
	"os"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/testenv"
)

// TestMain runs testenv.Main: the ExecRunner tests start processes, and
// one that wedges before exec fails instead of hanging.
func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }
