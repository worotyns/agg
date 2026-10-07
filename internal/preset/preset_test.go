package preset_test

import (
	"context"
	"testing"

	"github.com/worotyns/agg/internal/preset"
	"github.com/worotyns/agg/internal/testutil"
)

// Every preset must apply cleanly to an empty site, and applying it twice must change nothing.
func TestEveryPresetAppliesAndIsIdempotent(t *testing.T) {
	for _, p := range preset.All {
		t.Run(p.ID, func(t *testing.T) {
			f := testutil.New(t)
			ctx := context.Background()
			res, err := preset.Apply(ctx, f.St, f.Site, p.ID, "Europe/Warsaw")
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Created) == 0 || len(res.Skipped) != 0 || len(res.Setup) == 0 {
				t.Fatalf("first apply: %+v", res)
			}
			site, _ := f.St.GetSite(ctx, f.Site.ID)
			again, err := preset.Apply(ctx, f.St, site, p.ID, "")
			if err != nil || len(again.Created) != 0 {
				t.Fatalf("second apply must only skip: %+v %v", again, err)
			}
			alerts, _ := f.St.ListAlerts(ctx, f.Site.ID)
			for _, a := range alerts {
				if a.Schedule.Timezone != "Europe/Warsaw" {
					t.Errorf("alert %s timezone %q", a.Name, a.Schedule.Timezone)
				}
			}
		})
	}
}
