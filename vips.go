package vips

/*
#cgo pkg-config: vips
#include "vips.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"time"
	"unsafe"
)

const DEBUG = true

var (
	MARKER_JPEG = []byte{0xff, 0xd8}
	MARKER_PNG  = []byte{0x89, 0x50}
)

type ImageType int

const (
	UNKNOWN ImageType = iota
	JPEG
	PNG
)

type Interpolator int

const (
	BICUBIC Interpolator = iota
	BILINEAR
	NOHALO
)

type Extend int

const (
	EXTEND_BLACK Extend = C.VIPS_EXTEND_BLACK
	EXTEND_WHITE Extend = C.VIPS_EXTEND_WHITE
)

var interpolations = map[Interpolator]string{
	BICUBIC:  "bicubic",
	BILINEAR: "bilinear",
	NOHALO:   "nohalo",
}

func (i Interpolator) String() string { return interpolations[i] }

type Options struct {
	Height       int
	Width        int
	Crop         bool
	Enlarge      bool
	Extend       Extend
	Interpolator Interpolator
	Gravity      Gravity
	Quality      int
}

func init() {
	C.im_init_world(C.CString("go-vips"))
	C.vips_cache_set_max_mem(100 * 1048576) // 100Mb
	C.vips_cache_set_max(500)
}

func Resize(reader io.Reader, o Options) ([]byte, error) {
	started := time.Now()

	// start reading just 2 bytes
	buf := make([]byte, 2)
	_, err := reader.Read(buf)
	if err != nil {
		return nil, err
	}

	// detect (if possible) the file type
	typ := UNKNOWN
	switch {
	case buf[0] == MARKER_JPEG[0] && buf[1] == MARKER_JPEG[1]:
		typ = JPEG
	case buf[0] == MARKER_PNG[0] && buf[1] == MARKER_PNG[1]:
		typ = PNG
	default:
		return nil, errors.New("unknown image format")
	}

	// now we can read everything
	rest, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	buf = append(buf, rest...)

	// create an image instance
	in := C.vips_image_new()
	defer C.im_close(in)
	defer C.vips_error_clear()

	// defaults
	if o.Quality == 0 {
		o.Quality = 100
	}

	// feed it
	switch typ {
	case JPEG:
		C.vips_jpegload_buffer_rand(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &in)
	case PNG:
		C.vips_pngload_buffer_rand(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &in)
	}

	debug("options: %+v", o)

	// get WxH
	inWidth := int(in.Xsize)
	inHeight := int(in.Ysize)

	// prepare for factor
	factor := 0.0

	// image calculations
	switch {
	// Fixed width and height
	case o.Width > 0 && o.Height > 0:
		xf := float64(inWidth) / float64(o.Width)
		yf := float64(inHeight) / float64(o.Height)
		if o.Crop {
			factor = math.Min(xf, yf)
		} else {
			factor = math.Max(xf, yf)
		}
	// Fixed width, auto height
	case o.Width > 0:
		factor = float64(inWidth) / float64(o.Width)
		o.Height = int(math.Floor(float64(inHeight) / factor))
	// Fixed height, auto width
	case o.Height > 0:
		factor = float64(inWidth) / float64(o.Height)
		o.Width = int(math.Floor(float64(inWidth) / factor))
	// Identity transform
	default:
		factor = 1
		o.Width = inWidth
		o.Height = inHeight
	}

	// shrink
	shrink := int(math.Floor(factor))
	if shrink < 1 {
		shrink = 1
	}

	// residual
	residual := float64(shrink) / factor

	// Do not enlarge the output if the input width *or* height are already less than the required dimensions
	if !o.Enlarge {
		if inWidth < o.Width || inHeight < o.Height {
			factor = 1
			shrink = 1
			residual = 0
			o.Width = inWidth
			o.Height = inHeight
		}
	}

	// We don't use libjpeg shrink-on-load since we are not applying gamma correction
	shrinkOnLoad := 1
	if typ == JPEG {
		switch {
		case shrink >= 8:
			factor = factor / 8
			shrinkOnLoad = 8
		case shrink >= 4:
			factor = factor / 4
			shrinkOnLoad = 4
		case shrink >= 2:
			factor = factor / 2
			shrinkOnLoad = 2
		}
	}

	shrunkOnLoad := C.vips_image_new()
	defer C.im_close(shrunkOnLoad)

	if shrinkOnLoad > 1 {
		// Recalculate integral shrink and double residual
		factor = math.Max(factor, 1.0)
		shrink = int(math.Floor(factor))
		residual = float64(shrink) / factor
		// Reload input using shrink-on-load
		err := C.vips_jpegload_buffer_shrink(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &shrunkOnLoad, C.int(shrinkOnLoad))
		if err != 0 {
			return nil, resizeError()
		}
	} else {
		C.vips_copy_0(in, &shrunkOnLoad)
	}

	shrunk := C.vips_image_new()
	defer C.im_close(shrunk)

	if shrink > 1 {
		// Use vips_shrink with the integral reduction
		err := C.vips_shrink_0(shrunkOnLoad, &shrunk, C.double(float64(shrink)), C.double(float64(shrink)))
		if err != 0 {
			return nil, resizeError()
		}

		// Recalculate residual float based on dimensions of required vs shrunk images
		shrunkWidth := int(shrunk.Xsize)
		shrunkHeight := int(shrunk.Ysize)

		residualx := float64(o.Width) / float64(shrunkWidth)
		residualy := float64(o.Height) / float64(shrunkHeight)
		if o.Crop {
			residual = math.Max(residualx, residualy)
		} else {
			residual = math.Min(residualx, residualy)
		}
	} else {
		C.vips_copy_0(shrunkOnLoad, &shrunk)
	}

	// Use vips_affine with the remaining float part
	affined := C.vips_image_new()
	defer C.im_close(affined)

	if residual != 0 {
		// Create interpolator - "bilinear" (default), "bicubic" or "nohalo"
		interpolator := C.vips_interpolate_new(C.CString(o.Interpolator.String()))
		// Perform affine transformation
		err := C.vips_affine_interpolator(shrunk, &affined, C.double(residual), 0, 0, C.double(residual), interpolator)
		if err != 0 {
			return nil, resizeError()
		}
		C.g_object_unref(C.gpointer(interpolator))
	} else {
		C.vips_copy_0(shrunk, &affined)
	}

	// Crop/embed
	affinedWidth := int(affined.Xsize)
	affinedHeight := int(affined.Ysize)

	canvased := C.vips_image_new()
	defer C.im_close(canvased)

	if affinedWidth != o.Width || affinedHeight != o.Height {
		if o.Crop {
			// Crop
			left, top := sharpCalcCrop(affinedWidth, affinedHeight, o.Width, o.Height, o.Gravity)
			o.Width = int(math.Min(float64(affinedWidth), float64(o.Width)))
			o.Height = int(math.Min(float64(affinedHeight), float64(o.Height)))
			err := C.vips_extract_area_0(affined, &canvased, C.int(left), C.int(top), C.int(o.Width), C.int(o.Height))
			if err != 0 {
				return nil, resizeError()
			}
		} else {
			// Embed
			left := (o.Width - affinedWidth) / 2
			top := (o.Height - affinedHeight) / 2
			err := C.vips_embed_extend(affined, &canvased, C.int(left), C.int(top), C.int(o.Width), C.int(o.Height), C.int(o.Extend))
			if err != 0 {
				return nil, resizeError()
			}
		}
	} else {
		C.vips_copy_0(affined, &canvased)
	}

	// Always convert to sRGB colour space
	colourspaced := C.vips_image_new()
	defer C.im_close(colourspaced)

	C.vips_colourspace_0(canvased, &colourspaced, C.VIPS_INTERPRETATION_sRGB)

	// Finally save
	output := colourspaced

	length := C.size_t(0)
	ptr := unsafe.Pointer(&buf[0])
	C.vips_jpegsave_custom(output, &ptr, &length, 1, C.int(o.Quality), 0)

	if DEBUG { // avoid time of calculate the difference
		debug("done in %s", time.Since(started))
	}

	return C.GoBytes(ptr, C.int(length)), nil
}

func resizeError() error {
	s := C.GoString(C.vips_error_buffer())
	C.vips_error_clear()
	C.vips_thread_shutdown()
	return errors.New(s)
}

type Gravity int

const (
	CENTRE Gravity = iota
	NORTH
	EAST
	SOUTH
	WEST
)

func sharpCalcCrop(inWidth, inHeight, outWidth, outHeight int, gravity Gravity) (int, int) {
	left, top := 0, 0
	switch gravity {
	case NORTH:
		left = (inWidth - outWidth + 1) / 2
	case EAST:
		left = inWidth - outWidth
		top = (inHeight - outHeight + 1) / 2
	case SOUTH:
		left = (inWidth - outWidth + 1) / 2
		top = inHeight - outHeight
	case WEST:
		top = (inHeight - outHeight + 1) / 2
	default:
		left = (inWidth - outWidth + 1) / 2
		top = (inHeight - outHeight + 1) / 2
	}
	return left, top
}

func debug(format string, args ...interface{}) {
	if !DEBUG {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
