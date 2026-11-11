// Package pdftest makes small PDFs for tests.
package pdftest

import (
	"bytes"
	"fmt"
	"strings"
)

// Make returns a PDF with one page per string, each showing its string
// in Helvetica. Lines in a string go on separate lines of the page.
func Make(pages ...string) []byte {
	objs := make([]string, 0, 3+2*len(pages))
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	objs = append(objs,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, p := range pages {
		var cs strings.Builder
		cs.WriteString("BT /F1 14 Tf 72 720 Td 18 TL\n")
		for _, line := range strings.Split(p, "\n") {
			r := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`)
			fmt.Fprintf(&cs, "(%s) Tj T*\n", r.Replace(line))
		}
		cs.WriteString("ET")
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", cs.Len(), cs.String()))
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}
