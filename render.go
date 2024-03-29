package main

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

// Match: ![render-db6d08bb022ed12c2cc74d86d7a4707d.svg](/optional/path/to/render-db6d08bb022ed12c2cc74d86d7a4707d.svg)
// Capture group on the hash.
var renderedImageRegexp = regexp.MustCompile(`!\[render-.{32}\..+\]\(.*render-(.{32})\..+\)`)

var renderedHashRegexp = regexp.MustCompile(`<!-- hash:(.{8}) -->`)

// Match: ![alt text](filename.ext)
// Capture group on the filename.
var markdownImageRegexp = regexp.MustCompile(`!\[.*\]\((.+)\)`)

var (
	defaultRenderMode    = "normal"
	defaultRenderOptions = RenderOptions{Mode: defaultRenderMode}
)

type RenderOptions struct {
	Mode     string `json:"mode"` // Modes: normal, code-collapsed, image-collapsed, code-hidden
	Filename string `json:"filename"`
}

func (o *RenderOptions) Validate() error {
	if o.Mode == "" {
		o.Mode = defaultRenderMode
	}
	switch o.Mode {
	case "normal", "code-collapsed", "image-collapsed", "code-hidden":
	default:
		return errors.New("unsupported mode")
	}
	return nil
}

// Chunk represents a segment of a file
type Chunk struct {
	Lines          []string // Lines the chunk contains
	StartLineIndex int      // Index is relative to the input file
	EndLineIndex   int      // Index is relative to the input file
	CodeBlockIndex int      // Primarily for logging, to identify the problematic code block

	IsRenderable           bool
	Language               string
	ImageRelativeLineIndex int    // Where the image is located in the chunk. Index is relative to the chunk's lines.
	RenderedHash           string // If image has been rendered before, contains the hash of the code block previously used to render the image
	HasHashComment         bool
	CodeBlockContent       []string // The contents of the code block
	RenderOptions          RenderOptions
}

func (r *Chunk) ShouldRender() bool {
	if !r.IsRenderable {
		return false
	}

	// Support both a full hash (32 characters) and a short hash (8 characters)
	hash := r.HashContent()
	shortHash := hash[:8]
	if r.HashContent() != r.RenderedHash && shortHash != r.RenderedHash {
		return true
	}
	return false
}

func (r *Chunk) HashContent() string {
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(r.CodeBlockContent, "\n"))))
}

func (r *Chunk) Render(outputDir string, linkPrefix string) (fileName string, err error) {
	var content []byte
	if r.RenderOptions.Filename != "" {
		fileName = r.RenderOptions.Filename
	} else {
		fileName = "render-" + r.HashContent() + ".svg"
	}

	codeBlockContent := strings.Join(r.CodeBlockContent, "\n")
	switch r.Language {
	case "dot":
		ext := extFromFilename(fileName, []string{"svg", "png"}, "svg")
		content, err = runShellCommand("dot", []string{getDotFormatFlag(ext)}, strings.NewReader(codeBlockContent))
		if err != nil {
			return "", errors.Wrap(err, "render graphviz")
		}
	case "plantuml":
		ext := extFromFilename(fileName, []string{"svg", "png"}, "svg")
		content, err = runShellCommand("plantuml", []string{getPlantUMLFormatFlag(ext), "-pipe"}, strings.NewReader(codeBlockContent))
		if err != nil {
			return "", errors.Wrap(err, "render plantuml")
		}
	case "pikchr":
		content, err = runShellCommand("pikchr", []string{"--svg-only", "-"}, strings.NewReader(codeBlockContent))
		if err != nil {
			return "", errors.Wrap(err, "render pikchr")
		}
	default:
		return "", fmt.Errorf("unsupported type: %s", r.Language)
	}

	outputFilePath := path.Join(outputDir, fileName)
	f, err := os.Create(outputFilePath)
	if err != nil {
		return "", errors.Wrap(err, "create output file")
	}
	defer f.Close()
	f.Write(content)

	// Update the chunk's lines
	image := buildMarkdownImage(fileName, linkPrefix)
	if r.HasHashComment {
		hashComment := buildHashComment(r.HashContent()[:8])
		image = image + " " + hashComment
	}
	r.Lines[r.ImageRelativeLineIndex] = image

	return fileName, nil
}

func NewRenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render code blocks in markdown files",
		Long:  ``,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("no files specified as input")
			}
			return nil
		},
		RunE: renderCmd,
	}
	cmd.Flags().StringVar(&config.Render.OutputDir, "output-dir", "", "Directory to render code blocks to. If not specified, output will be rendered to the same directory as the input file.")
	cmd.Flags().StringVar(&config.Render.Languages, "languages", "", "(required) Languages to render. Comma-separated. Supported languages: [dot, plantuml, pikchr].")
	cmd.MarkFlagRequired("languages")
	cmd.Flags().StringVar(&config.Render.LinkPrefix, "link-prefix", "", "Prefix to use when linking to rendered files")
	return cmd
}

func renderCmd(cmd *cobra.Command, args []string) error {
	languages := strings.Split(config.Render.Languages, ",")
	for _, v := range args {
		err := processFile(v, languages, config.Render.OutputDir, config.Render.LinkPrefix)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("process file %s", v))
		}
	}
	return nil
}

func processFile(filePath string, types []string, outputDir string, linkPrefix string) error {
	err := validateFileExists(filePath)
	if err != nil {
		return err
	}

	// Read file into lines
	f, err := os.Open(filePath)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("open file %s", filePath))
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("read file %s", filePath))
	}
	inputFileContent := string(b)
	lines := strings.Split(inputFileContent, "\n")

	// Construct a lookup for O(1) access
	typeLookup := make(map[string]bool)
	for _, v := range types {
		typeLookup[v] = true
	}

	// Split the file into chunks. A chunk can represent either a normal
	// segment, or a renderable segment.
	var chunks []*Chunk
	var lastChunkIndex int
	for idx, line := range lines {
		// Skip ahead if these lines have been assigned a chunk already
		if idx < lastChunkIndex {
			continue
		}
		// Look for renderable code blocks
		if strings.HasPrefix(line, "```") {
			for k := range typeLookup {
				if strings.HasPrefix(line, fmt.Sprintf("```%s render", k)) {
					// Look at lines in and around the code
					// block to determine the renderable chunk.
					renderChunk, err := getRenderableChunk(lines, idx, k)
					if err != nil {
						return errors.Wrap(err, fmt.Sprintf("line %d: get renderable chunk", idx))
					}
					// Preceding lines not part of the renderable chunk are part of a
					// normal chunk; construct one and add it to our list of chunks.
					normalChunk := &Chunk{
						StartLineIndex: lastChunkIndex,
						EndLineIndex:   renderChunk.StartLineIndex - 1,
					}
					normalChunk.Lines = lines[normalChunk.StartLineIndex : normalChunk.EndLineIndex+1]
					chunks = append(chunks, normalChunk, renderChunk)
					lastChunkIndex = renderChunk.EndLineIndex + 1
					break
				}
			}
		}
	}
	if lastChunkIndex < len(lines) {
		// The rest of the file is a normal chunk
		normalChunk := &Chunk{
			StartLineIndex: lastChunkIndex,
			EndLineIndex:   len(lines) - 1,
		}
		normalChunk.Lines = lines[normalChunk.StartLineIndex : normalChunk.EndLineIndex+1]
		chunks = append(chunks, normalChunk)
	}

	// Render the renderable chunks and join the chunks back into a file
	var outputLines []string
	for _, chunk := range chunks {
		if chunk.ShouldRender() {
			imageFileName, err := chunk.Render(outputDir, linkPrefix)
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("line %d: render chunk", chunk.CodeBlockIndex+1))
			}
			fmt.Printf("[%s:%d] Rendered %s\n", filePath, chunk.CodeBlockIndex+1, imageFileName)
		}
		outputLines = append(outputLines, chunk.Lines...)
	}

	// Write to disk if file has changed
	outputContent := strings.Join(outputLines, "\n")
	if inputFileContent != outputContent {
		writer, err := os.OpenFile(filePath, os.O_WRONLY, 0666)
		if err != nil {
			return errors.Wrap(err, "open file for writing")
		}
		defer writer.Close()
		writer.WriteString(outputContent)
	}

	return nil
}

func getRenderableChunk(lines []string, codeBlockIndex int, language string) (*Chunk, error) {
	chunk := &Chunk{}
	chunk.IsRenderable = true
	chunk.Language = language
	chunk.CodeBlockIndex = codeBlockIndex

	fence := lines[codeBlockIndex]
	renderOptionsJSON := strings.TrimPrefix(fence, fmt.Sprintf("```%s render", language))
	if strings.HasPrefix(renderOptionsJSON, "{") && strings.HasSuffix(renderOptionsJSON, "}") {
		var renderOptions RenderOptions
		err := json.Unmarshal([]byte(renderOptionsJSON), &renderOptions)
		if err != nil {
			return nil, errors.Wrap(err, "unmarshal render options")
		}
		err = renderOptions.Validate()
		if err != nil {
			return nil, errors.Wrap(err, "validate render options")
		}
		chunk.RenderOptions = renderOptions
	} else {
		chunk.RenderOptions = defaultRenderOptions
	}

	// Add a hash comment if a custom filename is set
	if chunk.RenderOptions.Filename != "" {
		chunk.HasHashComment = true
	}

	var err error
	renderTemplateManager := RenderTemplateManager{}
	switch chunk.RenderOptions.Mode {
	case "normal":
		err = renderTemplateManager.Normal(lines, codeBlockIndex, chunk)
	case "code-collapsed":
		err = renderTemplateManager.CodeCollapsed(lines, codeBlockIndex, chunk)
	case "image-collapsed":
		err = renderTemplateManager.ImageCollapsed(lines, codeBlockIndex, chunk)
	case "code-hidden":
		err = renderTemplateManager.CodeHidden(lines, codeBlockIndex, chunk)
	default:
		return nil, errors.New("unsupported mode")
	}
	if err != nil {
		return nil, errors.Wrap(err, "parse render template")
	}

	return chunk, nil
}

func runShellCommand(command string, args []string, stdin io.Reader) (stdoutOutput []byte, err error) {
	cmd := exec.Command(command, args...)
	cmd.Stderr = os.Stderr
	cmd.Stdin = stdin
	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout
	err = cmd.Run()
	return stdout.Bytes(), err
}

func buildMarkdownImage(outputFilename, linkPrefix string) string {
	return fmt.Sprintf("![%s](%s)", outputFilename, linkPrefix+outputFilename)
}

func buildHashComment(hash string) string {
	return fmt.Sprintf("<!-- hash:%s -->", hash)
}

func extFromFilename(filename string, acceptedExtensions []string, defaultExtension string) string {
	ext := strings.TrimPrefix(filepath.Ext(filename), ".")
	for _, v := range acceptedExtensions {
		if ext == v {
			return ext
		}
	}
	return defaultExtension
}

func getDotFormatFlag(fileExtension string) string {
	switch fileExtension {
	case "png":
		return "-Tpng"
	case "svg":
		return "-Tsvg"
	default:
		return "-Tsvg"
	}
}

func getPlantUMLFormatFlag(fileExtension string) string {
	switch fileExtension {
	case "png":
		return "-tpng"
	case "svg":
		return "-tsvg"
	default:
		return "-tsvg"
	}
}
