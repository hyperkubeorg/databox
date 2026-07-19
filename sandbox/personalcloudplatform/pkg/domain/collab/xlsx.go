// xlsx.go — a minimal, dependency-free XLSX writer for spreadsheet
// export: sheet names and cell VALUES only (numbers as numbers,
// everything else as inline strings — no shared-string table, no
// styles). An .xlsx file is a zip of a few XML parts; this writes
// exactly the parts Excel, LibreOffice, and Google Sheets need to open
// the workbook.
package collab

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// WriteXLSX renders a document as an xlsx workbook.
func WriteXLSX(w io.Writer, doc GridDoc) error {
	zw := zip.NewWriter(w)
	add := func(name, content string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte(content))
		return err
	}

	var types, wbSheets, wbRels strings.Builder
	types.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	for i := range doc.Sheets {
		n := i + 1
		fmt.Fprintf(&types, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n)
		fmt.Fprintf(&wbSheets, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEscape(xlsxSheetName(doc.Sheets[i].Name, i)), n, n)
		fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n)
	}
	types.WriteString(`</Types>`)

	if err := add("[Content_Types].xml", types.String()); err != nil {
		return err
	}
	if err := add("_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>`+
		`</Relationships>`); err != nil {
		return err
	}
	if err := add("xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
		`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" `+
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`+
		`<sheets>`+wbSheets.String()+`</sheets></workbook>`); err != nil {
		return err
	}
	if err := add("xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
		wbRels.String()+`</Relationships>`); err != nil {
		return err
	}
	for i, sheet := range doc.Sheets {
		if err := add(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), worksheetXML(sheet)); err != nil {
			return err
		}
	}
	return zw.Close()
}

// worksheetXML renders one sheet's cell values.
func worksheetXML(sheet GridSheet) string {
	// Group cells by row for the <row> structure xlsx wants.
	rows := map[int]map[int]string{}
	maxRow := -1
	for key, cell := range sheet.Cells {
		v := gridCellValue(cell)
		if v == "" {
			continue
		}
		r, c, ok := parseCellKey(key)
		if !ok {
			continue
		}
		if rows[r] == nil {
			rows[r] = map[int]string{}
		}
		rows[r][c] = v
		if r > maxRow {
			maxRow = r
		}
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r := 0; r <= maxRow; r++ {
		cols := rows[r]
		if len(cols) == 0 {
			continue
		}
		fmt.Fprintf(&b, `<row r="%d">`, r+1)
		maxCol := -1
		for c := range cols {
			if c > maxCol {
				maxCol = c
			}
		}
		for c := 0; c <= maxCol; c++ {
			v, ok := cols[c]
			if !ok {
				continue
			}
			ref := XLSXColName(c) + strconv.Itoa(r+1)
			if _, err := strconv.ParseFloat(v, 64); err == nil {
				fmt.Fprintf(&b, `<c r="%s"><v>%s</v></c>`, ref, v)
			} else {
				fmt.Fprintf(&b, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, xmlEscape(v))
			}
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

// XLSXColName is the A, B, …, Z, AA column naming.
func XLSXColName(c int) string {
	name := ""
	c++
	for c > 0 {
		c--
		name = string(rune('A'+c%26)) + name
		c /= 26
	}
	return name
}

// xlsxSheetName sanitizes a tab name to Excel's rules (31 chars, no
// []:*?/\ characters, non-empty).
func xlsxSheetName(name string, idx int) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if len(name) > 31 {
		name = name[:31]
	}
	if name == "" {
		name = fmt.Sprintf("Sheet%d", idx+1)
	}
	return name
}

// xmlEscape escapes text for the XML parts.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
