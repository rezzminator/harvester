package harvest

import (
	"strings"
	"testing"
)

// TestLinkedInJobSalaryRange: a baseSalary stating only its minimum renders
// "from", only its maximum "up to", never as an exact amount.
func TestLinkedInJobSalaryRange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ value, want string }{
		{`{"minValue":90000,"unitText":"YEAR"}`, "**Salary:** from USD 90000 per year"},
		{`{"maxValue":120000,"unitText":"YEAR"}`, "**Salary:** up to USD 120000 per year"},
		{`{"minValue":90000,"maxValue":120000,"unitText":"YEAR"}`, "**Salary:** USD 90000 – 120000 per year"},
	} {
		job := `{"@type":"JobPosting","title":"Robot Technician","hiringOrganization":{"name":"Contoso"},` +
			`"baseSalary":{"currency":"USD","value":` + tc.value + `}}`
		extraction, _, ok := extractForSite("https://www.linkedin.com/jobs/view/4000000001/",
			linkedInInline(t, "", job))
		if !ok {
			t.Fatalf("%s: the job was not rendered", tc.value)
		}
		if !strings.Contains(extraction.markdown, tc.want) {
			t.Errorf("%s: the rendering lacks %q:\n%s", tc.value, tc.want, extraction.markdown)
		}
	}
}

// TestLinkedInJobEducationOfAnotherShapeIsKept: one educationRequirements
// value that is neither text nor an object with a credentialCategory is kept
// as stated beside the readable ones; the posting still renders.
func TestLinkedInJobEducationOfAnotherShapeIsKept(t *testing.T) {
	t.Parallel()
	job := `{"@type":"JobPosting","title":"Robot Technician","hiringOrganization":{"name":"Contoso"},` +
		`"educationRequirements":[{"credentialCategory":"bachelor degree"},7]}`
	extraction, _, ok := extractForSite("https://www.linkedin.com/jobs/view/4000000001/", linkedInInline(t, "", job))
	if !ok {
		t.Fatal("the job was not rendered")
	}
	if want := "- **Education:** bachelor degree, 7"; !strings.Contains(extraction.markdown, want) {
		t.Errorf("the rendering lacks %q:\n%s", want, extraction.markdown)
	}
}
