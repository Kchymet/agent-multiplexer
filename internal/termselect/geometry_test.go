package termselect

import (
	"reflect"
	"testing"
)

// These cells describe the displayed viewport, including the occupied second
// columns of 界 and 👩‍💻. Expected strings and spans are independent examples of
// the user's visible selection, not results from another range implementation.
func TestSelectionGeometry(t *testing.T) {
	rows := [][]Cell{
		{{"A", 1}, {"界", 2}, {}, {"e\u0301", 1}, {"Z", 1}},
		{{"x", 1}, {"👩‍💻", 2}, {}, {"y", 1}},
		{{"l", 1}, {"a", 1}, {"s", 1}, {"t", 1}},
	}
	tests := []struct {
		name  string
		a, b  Point
		text  string
		spans []Span
	}{
		{"ascii", Point{0, 0}, Point{0, 0}, "A", []Span{{0, 0, 1}}},
		{"wide leading half", Point{1, 0}, Point{1, 0}, "界", []Span{{0, 1, 3}}},
		{"wide trailing half", Point{2, 0}, Point{2, 0}, "界", []Span{{0, 1, 3}}},
		{"wide endpoints", Point{2, 0}, Point{3, 0}, "界e\u0301", []Span{{0, 1, 4}}},
		{"combining cluster", Point{3, 0}, Point{3, 0}, "e\u0301", []Span{{0, 3, 4}}},
		{"emoji trailing half", Point{2, 1}, Point{2, 1}, "👩‍💻", []Span{{1, 1, 3}}},
		{"multiline", Point{3, 0}, Point{1, 2}, "e\u0301Z\nx👩‍💻y\nla", []Span{{0, 3, 6}, {1, 0, 6}, {2, 0, 2}}},
		{"same column across rows", Point{2, 0}, Point{2, 1}, "界e\u0301Z\nx👩‍💻", []Span{{0, 1, 6}, {1, 0, 3}}},
		{"outside top left", Point{3, 1}, Point{-90, -20}, "A界e\u0301Z\nx👩‍💻y", []Span{{0, 0, 6}, {1, 0, 4}}},
		{"outside bottom right", Point{3, 1}, Point{999, 999}, "y\nlast", []Span{{1, 3, 6}, {2, 0, 6}}},
		{"drag into rail", Point{3, 0}, Point{-30, 1}, "e\u0301Z\nx", []Span{{0, 3, 6}, {1, 0, 1}}},
		{"padding inside row", Point{4, 1}, Point{4, 1}, " ", []Span{{1, 4, 5}}},
		{"padding to edge", Point{4, 1}, Point{5, 1}, "", []Span{{1, 4, 6}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := Selection{Spans: tt.spans, Text: tt.text}
			for _, ends := range [][2]Point{{tt.a, tt.b}, {tt.b, tt.a}} {
				if got := Select(rows, 6, ends[0], ends[1]); !reflect.DeepEqual(got, want) {
					t.Errorf("Select(%v, %v) = %#v, want %#v", ends[0], ends[1], got, want)
				}
			}
		})
	}
}

func TestSelectionViewportEdges(t *testing.T) {
	t.Run("wide glyph cut off by resize", func(t *testing.T) {
		rows := [][]Cell{{{"a", 1}, {"界", 2}, {}}}
		want := Selection{Spans: []Span{{0, 0, 2}}, Text: "a"}
		if got := Select(rows, 2, Point{}, Point{20, 0}); !reflect.DeepEqual(got, want) {
			t.Fatalf("clipped viewport = %#v, want %#v", got, want)
		}
	})
	t.Run("short history row and blank middle row", func(t *testing.T) {
		rows := [][]Cell{{{"a", 1}}, nil, {{"b", 1}}}
		want := Selection{Spans: []Span{{0, 0, 3}, {1, 0, 3}, {2, 0, 1}}, Text: "a\n\nb"}
		if got := Select(rows, 3, Point{}, Point{0, 2}); !reflect.DeepEqual(got, want) {
			t.Fatalf("history viewport = %#v, want %#v", got, want)
		}
	})
	for _, cols := range []int{-1, 0, 4} {
		if got := Select(nil, cols, Point{}, Point{}); !reflect.DeepEqual(got, Selection{}) {
			t.Errorf("empty viewport = %#v", got)
		}
	}
	for _, cols := range []int{-1, 0} {
		if got := Select([][]Cell{{{"a", 1}}}, cols, Point{}, Point{}); !reflect.DeepEqual(got, Selection{}) {
			t.Errorf("zero width viewport = %#v", got)
		}
	}
}
