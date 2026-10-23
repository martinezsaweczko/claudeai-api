package handlers

import (
	"context"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// FileUsageReporter records file access attempts (consumer-defined interface)
type FileUsageReporter interface {
	ReportFileUsage(ctx context.Context, file string, result int) error
}

// FileServerHandlerConfig holds dependencies for the file server handler
type FileServerHandlerConfig struct {
	Log   *slog.Logger
	TP    trace.TracerProvider
	MP    metric.MeterProvider
	Usage FileUsageReporter
	// Categories maps a URL prefix ("nacional_folder") to a filesystem directory
	Categories map[string]string
}

// FileServerHandler serves protected file downloads
type FileServerHandler struct {
	log        *slog.Logger
	tracer     trace.Tracer
	usage      FileUsageReporter
	categories map[string]string
	served     metric.Int64Counter
}

// NewFileServerHandler creates a new file server handler
func (c *FileServerHandlerConfig) NewFileServerHandler() (*FileServerHandler, error) {
	served, err := o11.NewBusinessCounter(c.MP, "files_served_total", "Files served by the download server per category and status")
	if err != nil {
		return nil, err
	}

	return &FileServerHandler{
		log:        c.Log,
		tracer:     c.TP.Tracer("http/handlers/fileserver"),
		usage:      c.Usage,
		categories: c.Categories,
		served:     served,
	}, nil
}

// RegisterRoutes registers one route per category: GET /{prefix}/{file}
func (h *FileServerHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	for prefix, dir := range h.categories {
		pattern := "GET /" + prefix + "/{file}"
		handler := h.serveFile(prefix, dir)
		if len(middlewares) > 0 {
			router.HandleFuncWithMiddleware(pattern, handler, middlewares...)
		} else {
			router.HandleFunc(pattern, handler)
		}
		h.log.Info("Registered file server route", "pattern", pattern, "dir", dir)
	}
}

// serveFile returns the handler for a single category.
//
//	@Summary      Download a kiosk file
//	@Description  Downloads a file from the configured kiosk folder. Requires a valid JWT token passed as the access_token query parameter.
//	@Tags         Quiosk
//	@Param        file path string true "File name"
//	@Param        access_token query string true "JWT token"
//	@Success      200 {file} binary "File content"
//	@Failure      401 {string} string "Missing or invalid JWT token"
//	@Failure      404 {string} string "File not found"
//	@Router       /nacional_folder/{file} [get]
//	@Router       /internacional_folder/{file} [get]
//	@Router       /magazine_folder/{file} [get]
func (h *FileServerHandler) serveFile(prefix, dir string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := h.tracer.Start(r.Context(), "handler.ServeFile",
			trace.WithAttributes(attribute.String("category", prefix)))
		defer span.End()
		r = r.WithContext(ctx)

		fileName := r.PathValue("file")

		// Reject path traversal attempts
		if filepath.Base(fileName) != fileName {
			h.respondWithStatus(w, r, prefix, fileName, http.StatusNotFound)
			return
		}

		fullPath := filepath.Join(dir, fileName)
		file, err := os.Open(fullPath)
		if err != nil {
			h.respondWithStatus(w, r, prefix, fileName, http.StatusNotFound)
			return
		}
		defer file.Close()

		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			h.respondWithStatus(w, r, prefix, fileName, http.StatusNotFound)
			return
		}

		contentType := mime.TypeByExtension(filepath.Ext(fileName))
		if contentType == "" {
			contentType = "application/pdf"
		}
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("Content-Type", contentType)

		// ServeContent sets Content-Length and supports range requests
		http.ServeContent(w, r, fileName, info.ModTime(), file)

		h.reportAndCount(r, prefix, fileName, http.StatusOK)
		h.log.Info("File served", "file", fileName, "category", prefix, "size", info.Size())
	}
}

// respondWithStatus writes an error status, reports the attempt and counts it
func (h *FileServerHandler) respondWithStatus(w http.ResponseWriter, r *http.Request, prefix, fileName string, status int) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(http.StatusText(status) + "\n"))
	h.reportAndCount(r, prefix, fileName, status)
}

// reportAndCount records the access attempt in the DB and the metrics counter
func (h *FileServerHandler) reportAndCount(r *http.Request, prefix, fileName string, status int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.served.Add(ctx, 1, metric.WithAttributes(
		attribute.String("category", prefix),
		attribute.Int("status", status),
	))
	if err := h.usage.ReportFileUsage(ctx, fileName, status); err != nil {
		h.log.Error("Failed to report file usage", "file", fileName, "error", err)
	}
}
