package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/avast/retry-go"
	"github.com/bogem/id3v2"
	"github.com/grafov/m3u8"
	"github.com/yyoshiki41/go-radiko"
	"github.com/yyoshiki41/radigo"
)

// reimplement some internal functions from
// https://github.com/yyoshiki41/radigo/blob/main/internal/download.go

var sem = make(chan struct{}, MaxConcurrency)

func bulkDownload(list []string, output string) error {
	var errFlag bool
	var wg sync.WaitGroup

	for _, v := range list {
		wg.Add(1)
		go func(link string) {
			defer wg.Done()

			var err error
			for i := 0; i < MaxRetryAttempts; i++ {
				sem <- struct{}{}
				err = downloadLink(link, output)
				<-sem
				if err == nil {
					break
				}
			}
			if err != nil {
				log.Printf("failed to download: %s", err)
				errFlag = true
			}
		}(v)
	}
	wg.Wait()

	if errFlag {
		return errors.New("lack of aac files")
	}
	return nil
}

func downloadLink(link, output string) error {
	resp, err := http.Get(link)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, fileName := filepath.Split(link)
	file, err := os.Create(filepath.Join(output, fileName))
	if err != nil {
		return err
	}

	_, err = io.Copy(file, resp.Body)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// downloadProgram manages the download for the given program
// in a go routine and notify the wg when finished
func downloadProgram(
	ctx context.Context, // the context for the request
	wg *sync.WaitGroup, // the wg to notify
	prog radiko.Prog, // the program metadata
	uri string, // the m3u8 URI for the program
	output *radigo.OutputConfig, // the file configuration
) {
	defer wg.Done()

	chunklist, err := getChunklistFromM3U8(uri)
	if err != nil {
		log.Printf("failed to get chunklist: %s", err)
		return
	}

	aacDir, err := output.TempAACDir()
	if err != nil {
		log.Printf("failed to create the aac dir: %s", err)
		return
	}
	defer os.RemoveAll(aacDir) // clean up

	if err := bulkDownload(chunklist, aacDir); err != nil {
		log.Printf("failed to download aac files: %s", err)
		return
	}

	concatedFile, err := radigo.ConcatAACFilesFromList(ctx, aacDir)
	if err != nil {
		log.Printf("failed to concat aac files: %s", err)
		return
	}

	switch output.AudioFormat() {
	case radigo.AudioFormatAAC:
		err = os.Rename(concatedFile, output.AbsPath())
	case radigo.AudioFormatMP3:
		err = radigo.ConvertAACtoMP3(ctx, concatedFile, output.AbsPath())
	default:
		log.Fatal("invalid file format")
	}

	if err != nil {
		log.Printf("failed to output a result file: %s", err)
		return
	}
	if err != nil {
		log.Printf("failed to open the output file: %s", err)
		return
	}
	tag, err := id3v2.Open(output.AbsPath(), id3v2.Options{Parse: true})
	if err != nil {
		log.Printf("error while opening the output file: %s", err)
	}
	defer tag.Close()

	// Set tags
	tag.SetTitle(output.FileBaseName)
	tag.SetArtist(prog.Pfm)
	tag.SetAlbum(prog.Title)
	tag.SetYear(prog.Ft[:4])
	tag.AddCommentFrame(id3v2.CommentFrame{
		Encoding:    id3v2.EncodingUTF8,
		Language:    "jpn",
		Description: prog.Info,
	})

	// write tag to the aac
	if err = tag.Save(); err != nil {
		log.Printf("error while saving a tag: %s", err)
	}

	// finish downloading the file
	log.Printf("+file saved: %s", output.AbsPath())
}

func Download(
	ctx context.Context,
	wg *sync.WaitGroup,
	client *radiko.Client,
	prog radiko.Prog,
	stationID string,
) error {
	title := prog.Title
	start := prog.Ft

	startTime, err := time.ParseInLocation(DatetimeLayout, start, Location)
	if err != nil {
		return fmt.Errorf("invalid start time format '%s': %s", start, err)
	}

	if startTime.After(CurrentTime) { // if it is in the future, skip
		log.Printf("the program is in the future [%s]%s (%s)", stationID, title, start)
		return nil
	}

	output, err := radigo.NewOutputConfig(
		fmt.Sprintf(
			"%s_%s_%s",
			startTime.In(Location).Format(OutputDatetimeLayout),
			stationID,
			title,
		),
		FileFormat,
	)
	if err != nil {
		return fmt.Errorf("failed to configure output: %s", err)
	}

	if err := output.SetupDir(); err != nil {
		return fmt.Errorf("failed to setup the output dir: %s", err)
	}

	if output.IsExist() {
		log.Printf("skip [%s]%s at %s", stationID, title, start)
		log.Printf("the output file already exists: %s", output.AbsPath())
		return nil
	}

	// detach the download job
	wg.Add(1)
	go func() {
		// try fetching the recording m3u8 uri
		var uri string
		err = retry.Do(
			func() error {
				uri, err = TimeshiftProgM3U8(ctx, client, stationID, prog)
				return err
			},
			retry.DelayType(func(n uint, err error, config *retry.Config) time.Duration {
				retry.DefaultDelay = 60 * time.Second
				delay := retry.BackOffDelay(n, err, config)
				log.Printf(
					"failed to get playlist.m3u8 for [%s]%s (%s): %s (retrying in %s)",
					stationID,
					title,
					start,
					err,
					delay,
				)
				// apply a default exponential back off strategy
				return delay
			}),
			retry.Attempts(MaxRetryAttempts),
			retry.Delay(InitialDelay),
		)
		if len(uri) == 0 {
			wg.Done()
			return
		}
		log.Printf("start downloading [%s]%s (%s): %s", stationID, title, start, uri)
		go downloadProgram(ctx, wg, prog, uri, output)
	}()
	return nil
}

// GetURI returns uri generated by parsing m3u8.
func getURI(input io.Reader) (string, error) {
	playlist, listType, err := m3u8.DecodeFrom(input, true)
	if err != nil || listType != m3u8.MASTER {
		return "", err
	}
	p := playlist.(*m3u8.MasterPlaylist)

	if p == nil || len(p.Variants) != 1 || p.Variants[0] == nil {
		return "", errors.New("invalid m3u8 format")
	}
	return p.Variants[0].URI, nil
}

// GetChunklist returns a slice of uri string.
func getChunklist(input io.Reader) ([]string, error) {
	playlist, listType, err := m3u8.DecodeFrom(input, true)
	if err != nil || listType != m3u8.MEDIA {
		return nil, err
	}
	p := playlist.(*m3u8.MediaPlaylist)

	var chunklist []string
	for _, v := range p.Segments {
		if v != nil {
			chunklist = append(chunklist, v.URI)
		}
	}
	return chunklist, nil
}

// getChunklistFromM3U8 returns a slice of url.
func getChunklistFromM3U8(uri string) ([]string, error) {
	resp, err := http.Get(uri)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return getChunklist(resp.Body)
}

func TimeshiftProgM3U8(
	ctx context.Context,
	client *radiko.Client,
	stationID string,
	prog radiko.Prog,
) (string, error) {
	var req *http.Request
	var err error
	areaID := getArea(stationID)

	log.Printf("area-id: %s", areaID)
	token := GetToken(ctx, client, areaID)
	log.Printf("token: %s", token)

	u := *client.URL
	u.Path = path.Join(client.URL.Path, "v2/api/ts/playlist.m3u8")
	// Add query parameters
	urlQuery := u.Query()
	params := map[string]string{
		"station_id": stationID,
		"ft":         prog.Ft,
		"to":         prog.To,
		"l":          "15", // required?
	}
	for k, v := range params {
		urlQuery.Set(k, v)
	}
	u.RawQuery = urlQuery.Encode()
	req, _ = http.NewRequest("POST", u.String(), nil)
	req = req.WithContext(ctx)
	headers := map[string]string{
		"User-Agent":          "Dalvik/2.1.0 (Linux; U; Android 11.0.0; D5833/RQ1A.201205.011)",
		RadikoAreaIDHeader:    areaID,
		RadikoAuthTokenHeader: token,
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	log.Println(resp.Status)
	return getURI(resp.Body)
}
