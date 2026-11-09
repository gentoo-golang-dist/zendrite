package thumbnailer

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/mediaapi/types"
)

func TestReadWebP(t *testing.T) {
	for _, animated := range []bool{false, true} {
		name, size, red := "still", 2, image.Pt(0, 0)
		if animated {
			name, size, red = "animated", 4, image.Pt(2, 2)
		}
		t.Run(name, func(t *testing.T) {
			img, err := readFile("testdata/" + name + ".webp")
			require.NoError(t, err)
			require.Equal(t, image.Rect(0, 0, size, size), img.Bounds())
			require.Equal(t, color.NRGBA{R: 255, A: 255}, color.NRGBAModel.Convert(img.At(red.X, red.Y)))
			if animated {
				// The first frame occupies the lower-right quadrant. Later frames are blue.
				_, _, _, alpha := img.At(0, 0).RGBA()
				require.Zero(t, alpha)
			}
			// Exercise the JPEG thumbnail path, not just decoding.
			path := filepath.Join(t.TempDir(), "thumbnail.jpg")
			require.NoError(t, writeFile(img, path, jpegThumbnail))
			thumbnail, err := readFile(path)
			require.NoError(t, err)
			require.Equal(t, img.Bounds(), thumbnail.Bounds())
		})
	}
}

func TestReadOtherImageFormats(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for _, format := range []string{"png", "jpeg", "gif"} {
		t.Run(format, func(t *testing.T) {
			var data bytes.Buffer
			var err error
			switch format {
			case "png":
				err = png.Encode(&data, img)
			case "jpeg":
				err = jpeg.Encode(&data, img, nil)
			case "gif":
				err = gif.Encode(&data, img, nil)
			}
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "image")
			require.NoError(t, os.WriteFile(path, data.Bytes(), 0o600))
			decoded, err := readFile(path)
			require.NoError(t, err)
			require.Equal(t, img.Bounds(), decoded.Bounds())
		})
	}
}

func TestReadInvalidWebP(t *testing.T) {
	data, err := os.ReadFile("testdata/animated.webp")
	require.NoError(t, err)
	for _, data := range [][]byte{nil, []byte("RIFF"), []byte("RIFF0000WEBPbroken"), data[:40]} {
		path := filepath.Join(t.TempDir(), "invalid.webp")
		require.NoError(t, os.WriteFile(path, data, 0o600))
		_, err := readFile(path)
		require.Error(t, err, "input length %d", len(data))
	}
}

// PNG and GIF files get PNG thumbnails, which keep transparency; other
// images get JPEG thumbnails.
func TestThumbnailContentType(t *testing.T) {
	for source, want := range map[types.ContentType]types.ContentType{
		"image/png":                 pngThumbnail,
		"image/PNG; charset=binary": pngThumbnail,
		"image/apng":                pngThumbnail,
		"image/gif":                 pngThumbnail,
		"image/jpeg":                jpegThumbnail,
		"image/webp":                jpegThumbnail,
		"":                          jpegThumbnail,
	} {
		require.Equal(t, want, thumbnailContentType(source), "source %q", source)
	}

	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 128})
	path := filepath.Join(t.TempDir(), "thumbnail")
	require.NoError(t, writeFile(img, path, pngThumbnail))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "\x89PNG", string(data[:4]), "a PNG thumbnail is encoded as PNG")
	thumbnail, err := readFile(path)
	require.NoError(t, err)
	pixel, ok := color.NRGBAModel.Convert(thumbnail.At(0, 0)).(color.NRGBA)
	require.True(t, ok)
	require.Equal(t, uint8(128), pixel.A, "a PNG thumbnail keeps transparency")
}
