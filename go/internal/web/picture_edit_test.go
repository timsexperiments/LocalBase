package web

import (
	"regexp"
	"strings"
	"testing"
)

// The Get list says which models can edit a picture, from the catalog (image-to-image), and the
// model page names capabilities in words. Studio's Edit sends people to /app/models/get?filter=images.
func TestGetSaysWhichCanEditPictures(t *testing.T) {
	u := shellUI(t, nil, "")
	main := mainRe.FindString(get(u, "/app/models/get?filter=images").Body.String())
	rows := regexp.MustCompile(`(?s)<li class="model-row" id="cat-([^"]+)">.*?</li>`).FindAllStringSubmatch(main, -1)
	if len(rows) == 0 {
		t.Fatalf("no image rows: %.400s", main)
	}
	editors := 0
	for _, r := range rows {
		says := strings.Contains(r[0], "Can edit pictures")
		s := u.d.Catalog.ByID(r[1])
		can := s != nil && s.HasCapability("image-to-image")
		if says != can {
			t.Errorf("%s: says it can edit pictures %v, catalog says %v", r[1], says, can)
		}
		if says {
			editors++
		}
	}
	if editors == 0 {
		t.Error("no image row says it can edit pictures: the catalog has none, or the line is gone")
	}
	page := get(u, "/app/models/m/juggernaut-xl-v9").Body.String()
	if !strings.Contains(page, `<li class="badge">Image editing</li>`) || strings.Contains(page, `<li class="badge">image-to-image</li>`) {
		t.Errorf("the model page names its capabilities by id: %.300s", page[strings.Index(page, `aria-label="Capabilities"`)+0:])
	}
}
