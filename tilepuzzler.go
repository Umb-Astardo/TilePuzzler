package main

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
	"github.com/toqueteos/webbrowser"

	"encoding/json"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/nfnt/resize"
)

// Configure the WebSocket upgrader
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Allow all connections for this simple proxy
		return true
	},
}

//go:embed tilepuzzler.html
var embeddedFS embed.FS

func main() {
	// Ensure the images directory exists
	if err := os.MkdirAll("images", 0755); err != nil {
		log.Fatalf("Failed to create images directory: %v", err)
	}

	http.HandleFunc("/", serveSPA)
	http.HandleFunc("/exportPuzzle", exportPuzzleHandler)
	http.HandleFunc("/uploadPuzzle", uploadPuzzleHandler)
	imagesHandler := http.StripPrefix("/images/", http.FileServer(http.Dir("./images")))
	http.Handle("/images/", imagesHandler)

	port := "8080"
	fmt.Printf("Starting TilePuzzler server on http://localhost:%s\n", port)
	webbrowser.Open("http://localhost:" + port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// Helper: Resize image
func resizeImage(img image.Image, width, height int) image.Image {
	return resize.Resize(uint(width), uint(height), img, resize.Lanczos3)
}

type ExportPayload struct {
	Folder     string            `json:"folder"`
	Placements map[string]string `json:"placements"` // "row,col":"filename"
}

func serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Check if an external tilepuzzler.html exists
	_, err := os.Stat("tilepuzzler.html")
	if err == nil {
		// If it exists, serve the external file.
		// This allows for easy development and customization without rebuilding.
		log.Println("Serving external tilepuzzler.html")
		http.ServeFile(w, r, "tilepuzzler.html")
		return
	}

	// If the external file doesn't exist, serve the embedded version.
	log.Println("Serving embedded tilepuzzler.html")
	file, err := embeddedFS.Open("tilepuzzler.html")
	if err != nil {
		log.Printf("FATAL: Could not open embedded tilepuzzler.html: %v", err)
		http.Error(w, "Internal Server Error: Embedded file not found.", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		log.Printf("FATAL: Could not stat embedded tilepuzzler.html: %v", err)
		http.Error(w, "Internal Server Error: Cannot stat embedded file.", http.StatusInternalServerError)
		return
	}

	// Read the file content into a buffer to create an io.ReadSeeker, which http.ServeContent needs.
	content, err := io.ReadAll(file)
	if err != nil {
		log.Printf("FATAL: Could not read embedded tilepuzzler.html: %v", err)
		http.Error(w, "Internal Server Error: Cannot read embedded file.", http.StatusInternalServerError)
		return
	}
	reader := bytes.NewReader(content)

	// Use http.ServeContent to handle caching headers correctly.
	http.ServeContent(w, r, "tilepuzzler.html", stat.ModTime(), reader)
}

func exportPuzzleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var payload ExportPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Printf("Exporting %s\n", payload.Folder)
	basePath := filepath.Join("images", payload.Folder)
	tileSize := 512

	// Determine canvas size
	var maxRow, maxCol int
	for pos := range payload.Placements {
		var r, c int
		fmt.Sscanf(pos, "%d,%d", &r, &c)
		if r > maxRow {
			maxRow = r
		}
		if c > maxCol {
			maxCol = c
		}
	}
	canvasW := (maxCol + 1) * tileSize
	canvasH := (maxRow + 1) * tileSize

	dst := image.NewRGBA(image.Rect(0, 0, canvasW, canvasH))

	for pos, filename := range payload.Placements {
		var r, c int
		fmt.Sscanf(pos, "%d,%d", &r, &c)

		tilePath := filepath.Join(basePath, "pieces", filename)
		fmt.Printf("adding %s\n", filename)

		tileFile, err := os.Open(tilePath)
		if err != nil {
			log.Printf("Failed to open tile %s: %v", filename, err)
			continue
		}
		img, _, err := image.Decode(tileFile)
		tileFile.Close()
		if err != nil {
			log.Printf("Failed to decode tile %s: %v", filename, err)
			continue
		}

		pt := image.Pt(c*tileSize, r*tileSize)
		draw.Draw(dst, image.Rectangle{Min: pt, Max: pt.Add(img.Bounds().Size())}, img, image.Point{}, draw.Over)
	}
	fmt.Printf("returning completed image\n")

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `attachment; filename="puzzle.png"`)
	if err := png.Encode(w, dst); err != nil {
		http.Error(w, "Failed to encode PNG: "+err.Error(), http.StatusInternalServerError)
	}
}

func uploadPuzzleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	// Parse the multipart form
	err := r.ParseMultipartForm(10 << 20) // 10 MB
	if err != nil {
		http.Error(w, "Error parsing multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Get puzzle name
	puzzleName := r.FormValue("name")
	if puzzleName == "" {
		http.Error(w, "Puzzle name is required", http.StatusBadRequest)
		return
	}

	// Get columns value
	columnsStr := r.FormValue("columns")
	columns, err := strconv.Atoi(columnsStr)
	if err != nil || columns <= 0 {
		http.Error(w, "Invalid number of columns", http.StatusBadRequest)
		return
	}

	// Get the image file
	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Error retrieving the file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Decode the image
	img, _, err := image.Decode(file)
	if err != nil {
		http.Error(w, "Error decoding image: "+err.Error(), http.StatusBadRequest)
		return
	}

	const tileSize = 512

	// Resize the image
	originalBounds := img.Bounds()
	originalWidth := originalBounds.Dx()
	originalHeight := originalBounds.Dy()

	targetWidth := tileSize * columns
	aspectRatio := float64(originalWidth) / float64(originalHeight)
	targetHeight := int(float64(targetWidth) / aspectRatio)

	resizedImg := resize.Resize(uint(targetWidth), uint(targetHeight), img, resize.Lanczos3)

	// Create puzzle directory
	puzzleDirName := toSnakeCase(puzzleName)
	puzzlePath := filepath.Join("images", puzzleDirName)
	if err := os.MkdirAll(filepath.Join(puzzlePath, "pieces"), 0755); err != nil {
		http.Error(w, "Error creating puzzle directory: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Save original image as index.jpg
	indexPath := filepath.Join(puzzlePath, "index.jpg")
	indexFile, err := os.Create(indexPath)
	if err != nil {
		http.Error(w, "Error creating index.jpg: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer indexFile.Close()
	// We need to encode the resized image
	if err := jpeg.Encode(indexFile, resizedImg, nil); err != nil {
		http.Error(w, "Error saving index.jpg: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Slice the image into tiles
	bounds := resizedImg.Bounds()
	cols := (bounds.Max.X + tileSize - 1) / tileSize
	rows := (bounds.Max.Y + tileSize - 1) / tileSize

	type PieceInfo struct {
		File string `json:"file"`
	}
	var pieces []PieceInfo
	solution := make(map[string]string)

	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			x0 := c * tileSize
			y0 := r * tileSize
			x1 := x0 + tileSize
			y1 := y0 + tileSize

			if x1 > bounds.Max.X {
				x1 = bounds.Max.X
			}
			if y1 > bounds.Max.Y {
				y1 = bounds.Max.Y
			}

			tileRect := image.Rect(x0, y0, x1, y1)
			tileImg := image.NewRGBA(tileRect)
			draw.Draw(tileImg, tileRect, resizedImg, image.Point{x0, y0}, draw.Src)

			// Save the tile
			tileName := fmt.Sprintf("image_%04d.png", len(pieces))
			tilePath := filepath.Join(puzzlePath, "pieces", tileName)
			tileFile, err := os.Create(tilePath)
			if err != nil {
				http.Error(w, "Error creating tile file: "+err.Error(), http.StatusInternalServerError)
				return
			}
			png.Encode(tileFile, tileImg)
			tileFile.Close()

			pieces = append(pieces, PieceInfo{File: tileName})
			solution[fmt.Sprintf("%d,%d", r, c)] = tileName
		}
	}

	// Create manifest.json
	type Manifest struct {
		Pieces   []PieceInfo       `json:"pieces"`
		Solution map[string]string `json:"solution"`
	}
	manifest := Manifest{Pieces: pieces, Solution: solution}
	manifestPath := filepath.Join(puzzlePath, "manifest.json")
	manifestFile, err := os.Create(manifestPath)
	if err != nil {
		http.Error(w, "Error creating manifest.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer manifestFile.Close()
	json.NewEncoder(manifestFile).Encode(manifest)

	// Update imageIndex.json
	imageIndexMutex.Lock()
	defer imageIndexMutex.Unlock()

	type ImageIndex struct {
		Images []struct {
			Name   string `json:"name"`
			Folder string `json:"folder"`
			Rows   int    `json:"rows"`
			Cols   int    `json:"cols"`
			Tl     string `json:"tl"`
		} `json:"images"`
	}
	imageIndexPath := filepath.Join("images", "imageIndex.json")
	var imageIndex ImageIndex
	imageIndexFile, err := os.ReadFile(imageIndexPath)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, "Error reading imageIndex.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(imageIndexFile) > 0 {
		if err := json.Unmarshal(imageIndexFile, &imageIndex); err != nil {
			http.Error(w, "Error parsing imageIndex.json: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	newImage := struct {
		Name   string `json:"name"`
		Folder string `json:"folder"`
		Rows   int    `json:"rows"`
		Cols   int    `json:"cols"`
		Tl     string `json:"tl"`
	}{
		Name:   puzzleName,
		Folder: puzzleDirName,
		Rows:   rows,
		Cols:   cols,
		Tl:     "image_0000.png", // Assuming the first tile is the top-left
	}
	imageIndex.Images = append(imageIndex.Images, newImage)

	updatedImageIndex, err := json.MarshalIndent(imageIndex, "", "  ")
	if err != nil {
		http.Error(w, "Error marshalling imageIndex.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(imageIndexPath, updatedImageIndex, 0644); err != nil {
		http.Error(w, "Error writing imageIndex.json: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func toSnakeCase(s string) string {
	var result strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				result.WriteRune('_')
			}
			result.WriteRune(unicode.ToLower(r))
		} else if r == ' ' || r == '-' {
			result.WriteRune('_')
		} else {
			result.WriteRune(r)
		}
	}
	return result.String()
}

var (
	imageIndexMutex sync.Mutex
)
