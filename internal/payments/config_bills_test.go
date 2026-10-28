package payments

import "testing"

func TestBillDownloadsRequireExplicitBooleanConfiguration(t *testing.T) {
	for _, value := range []string{"", "false", "true", "yes", "1", "TRUE"} {
		t.Run("value_"+value, func(t *testing.T) {
			t.Setenv("BILL_DOWNLOADS_ENABLED", value)
			enabled, err := BillDownloadsEnabledFromEnvironment()
			switch value {
			case "true":
				if err != nil || !enabled {
					t.Fatal("explicit flag ignored")
				}
			case "", "false":
				if err != nil || enabled {
					t.Fatal("downloads enabled by default")
				}
			default:
				if err == nil || enabled {
					t.Fatal("ambiguous flag accepted")
				}
			}
		})
	}
}
