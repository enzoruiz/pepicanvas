package canvas

// Fit places an object of natural size width x height, both positive, inside
// area: at its natural size when it fits, otherwise scaled down to the
// largest size that fits with its aspect ratio kept (rounded to the nearest
// pixel, each side at least 1). The object is centered, rounding toward the
// top left, and not rotated.
func Fit(area Dimensions, width, height int) Transform {
	w, h := width, height
	if w > area.Width || h > area.Height {
		// Compare the ratios in integers: the side that overflows the most
		// takes the full extent of the area.
		if width*area.Height > height*area.Width {
			w, h = area.Width, max(scale(height, area.Width, width), 1)
		} else {
			w, h = max(scale(width, area.Height, height), 1), area.Height
		}
	}
	return Transform{X: (area.Width - w) / 2, Y: (area.Height - h) / 2, Width: w, Height: h}
}

// scale returns value * num / den rounded half up, for non-negative operands.
func scale(value, num, den int) int {
	return (2*value*num + den) / (2 * den)
}
