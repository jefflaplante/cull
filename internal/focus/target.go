// Package focus decides which native-resolution regions the model judges focus
// on: the intended subject (a detected face, or a box the model located) and the
// regions where focus most likely landed.
package focus

import "image"

const (
	MinSubject = 768  // smallest subject crop side, native px
	MaxSubject = 1536 // largest; stays under the ~1568px the APIs pass through unscaled
)

// Target is what should be sharp, in display-oriented native pixels.
type Target struct {
	Center image.Point
	Size   int           // extent the crop should cover
	Source string        // "face" or "model"
	Pupils []image.Point // a face's pupils found (0-2), native pixels
}

// SubjectRect is a square crop centred on the target, side clamped to
// [MinSubject, MaxSubject] and to the frame, shifted to lie inside the frame.
func SubjectRect(t Target, w, h int) image.Rectangle {
	return centered(t.Center, min(max(t.Size, MinSubject), MaxSubject), w, h)
}

// centered returns a side×side square around c, moved inside a w×h frame and
// shrunk only if the frame itself is smaller.
func centered(c image.Point, side, w, h int) image.Rectangle {
	side = min(side, w, h)
	x0 := min(max(c.X-side/2, 0), w-side)
	y0 := min(max(c.Y-side/2, 0), h-side)
	return image.Rect(x0, y0, x0+side, y0+side)
}
