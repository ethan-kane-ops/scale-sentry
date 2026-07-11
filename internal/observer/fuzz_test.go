package observer

import (
	"testing"
)

func FuzzParseReportLog(f *testing.F) {
	valid, err := MarshalReportLine(Report{})
	if err != nil {
		f.Fatalf("marshal seed report: %v", err)
	}
	f.Add([]byte("startup noise\n" + valid + "\ntrailing noise\n"))
	f.Add([]byte(ReportLogPrefix + "{"))
	f.Add([]byte(ReportLogPrefix + "null"))
	f.Add([]byte("no marker line at all\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseReportLog(data)
	})
}
