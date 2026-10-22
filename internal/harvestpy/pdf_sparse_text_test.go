package harvestpy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPDFKeepsNativeTextWhenOCRReturnsNothing(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "converter.py"), ConverterSource(), 0o600); err != nil {
		t.Fatal(err)
	}
	const probe = `
import sys, types
sys.path.insert(0, sys.argv[1])
import converter as c
class Page:
    number = 0
    def get_text(self, *args, **kwargs): return self.text
    def get_image_info(self): return [{}] if self.image else []
class Document:
    page_count = 1
    def __enter__(self): return self
    def __exit__(self, *args): pass
    def __iter__(self): return iter([page])
    def __getitem__(self, index): return page
page = Page()
sys.modules['pymupdf'] = types.SimpleNamespace(open=lambda _: Document(), TEXTFLAGS_TEXT=0, TEXT_PRESERVE_LIGATURES=0)
c.page_features = lambda _: {}
c.ocr_page_needed = lambda _: True
c.choose_ocr_script = lambda *args: ('latin', 'fixture')
c._ocr_pages = lambda *args: ({0: ''}, ['OCR found no text on page(s) 1'])
for text, image, readable in [
    ('Dummy PDF file', False, True),
    ('42', False, True),
    ('A readable native text layer that OCR failed to recognize.', True, True),
    ('1', False, False),
    ('Short caption', True, False),
    ('', True, False),
    ('\ufffd\x03', False, False),
]:
    page.text, page.image = text, image
    result = c.convert({'path': 'fixture.pdf', 'kind': 'pdf'})
    assert result['ok'] == readable, (text, image, result)
    if readable:
        assert text in result['markdown'], result
        assert '_Converter gap: OCR found no text' in result['markdown'], result
        assert 'using the native text layer instead' in result['markdown'], result
    else:
        assert result['error_class'] == 'OCRUnavailable', result
print('native text retained; unreadable scans remain failures')
`
	cmd := exec.Command(python, "-c", probe, dir)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PDF native text fallback: %v\n%s", err, out)
	}
}
