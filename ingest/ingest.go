package ingest

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"scratchdb/config"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/jeremywohl/flatten"
	"github.com/spyzhov/ajson"
	"golang.org/x/crypto/acme/autocert"
)

type FileIngest struct {
	Config config.Config

	app     *fiber.App
	writers map[string]*FileWriter
}

func NewFileIngest(config config.Config) FileIngest {
	i := FileIngest{
		Config: config,
	}
	i.app = fiber.New()

	i.writers = make(map[string]*FileWriter)
	return i
}

func (i *FileIngest) Index(c *fiber.Ctx) error {
	return c.SendString("ok")
}

// TODO: Common pool of writers and uploaders across all API keys, rather than just one
// TODO: Start the uploading process independent of whether new data has been inserted for that API key
func (i *FileIngest) InsertData(c *fiber.Ctx) error {
	api_key := c.Get("X-API-KEY")
	// TODO: validate api key upon insert

	input := c.Body()

	// Ensure JSON is valid
	if !json.Valid(input) {
		return fiber.ErrBadRequest
	}

	root, err := ajson.Unmarshal(input)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	data_path := "$"
	table_name := c.Get("X-SCRATCHDB-TABLE")
	if table_name == "" {
		table, err := root.GetKey("table")
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		table_name = table.String()
		data_path = "$.data"
	}

	x, err := root.JSONPath(data_path)
	if err != nil {
		return err
	}

	flat, err := flatten.FlattenString(x[0].String(), "", flatten.UnderscoreStyle)
	if err != nil {
		return err
	}

	dir := filepath.Join(i.Config.Ingest.Data, api_key, table_name)
	writer, ok := i.writers[dir]
	if !ok {
		writer = NewFileWriter(dir, i.Config.Ingest.MaxAgeSeconds, i.Config.Ingest.MaxSizeBytes, i.Config.AWS)
		i.writers[dir] = writer
	}

	err = writer.Write(flat)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.SendString("ok")
}

func (i *FileIngest) runSSL() {

	// Certificate manager
	m := &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		// Replace with your domain
		HostPolicy: autocert.HostWhitelist(i.Config.SSL.Hostnames...),
		// Folder to store the certificates
		Cache: autocert.DirCache("./certs"),
	}

	// TLS Config
	cfg := &tls.Config{
		// Get Certificate from Let's Encrypt
		GetCertificate: m.GetCertificate,
		// By default NextProtos contains the "h2"
		// This has to be removed since Fasthttp does not support HTTP/2
		// Or it will cause a flood of PRI method logs
		// http://webconcepts.info/concepts/http-method/PRI
		NextProtos: []string{
			"http/1.1", "acme-tls/1",
		},
	}
	ln, err := tls.Listen("tcp", ":443", cfg)
	if err != nil {
		panic(err)
	}

	if err := i.app.Listener(ln); err != nil {
		log.Panic(err)
	}
}

func (i *FileIngest) Start() {
	// TODO: recover from non-graceful shutdown. What if there are files left on disk when we restart?

	i.app.Use(logger.New())

	i.app.Get("/", i.Index)
	i.app.Post("/data", i.InsertData)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		_ = <-c
		fmt.Println("Gracefully shutting down...")

		// TODO: set readtimeout to something besides 0 to close keepalive connections
		_ = i.app.Shutdown()
	}()

	if i.Config.SSL.Enabled {
		i.runSSL()
	} else {
		if err := i.app.Listen(":" + i.Config.Ingest.Port); err != nil {
			log.Panic(err)
		}
	}

	fmt.Println("Running cleanup tasks...")

	// Closing writers
	for name, writer := range i.writers {
		log.Println("Closing writer", name)
		err := writer.Close()
		if err != nil {
			log.Println(err)
		}
	}

}
