package speedtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/gocarina/gocsv"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/librespeed/speedtest-cli/defs"
	"github.com/librespeed/speedtest-cli/report"
)

const (
	// the default ping count for measuring ping and jitter
	pingCount = 10
)

// doSpeedTest is where the actual speed test happens
func CliSpeedTest(testOpts *defs.TestOptions, c *cli.Context, silent bool) error {
	if serverCount := len(testOpts.ServerList); serverCount > 1 {
		log.Infof("Testing against %d servers", serverCount)
	}

	var reps []report.Report

	// fetch current user's IP info
	for _, currentServer := range testOpts.ServerList {
		// get telemetry level
		currentServer.TLog.SetLevel(testOpts.TelemetryServer.GetLevel())

		u, err := currentServer.GetURL()
		if err != nil {
			log.Errorf("Failed to get server URL: %s", err)
			return err
		}

		log.Infof("Selected server: %s [%s]", currentServer.Name, u.Hostname())

		if sponsorMsg := currentServer.Sponsor(); sponsorMsg != "" {
			log.Infof("Sponsored by: %s", sponsorMsg)
		}

		if !currentServer.IsUp() {
			log.Infof("Selected server %s (%s) is not responding at the moment, try again later", currentServer.Name, u.Hostname())
		}

		ispInfo, err := currentServer.GetIPInfo(string(testOpts.DistanceUnit))
		if err != nil {
			log.Errorf("Failed to get IP info: %s", err)
			return err
		}
		log.Infof("You're testing from: %s", ispInfo.ProcessedString)

		// get ping and jitter value
		var pb *spinner.Spinner
		if !silent {
			pb = spinner.New(spinner.CharSets[11], 100*time.Millisecond)
			pb.Prefix = "Pinging server...  "
			pb.Start()
		}

		p, jitter, err := currentServer.ICMPPingAndJitter(pingCount, testOpts.SourceIP, testOpts.Network)
		if err != nil {
			log.Errorf("Failed to get ping and jitter: %s", err)
			return err
		}

		if pb != nil {
			pb.FinalMSG = fmt.Sprintf("Ping: %.2f ms\tJitter: %.2f ms\n", p, jitter)
			pb.Stop()
		}

		// get download value
		var downloadValue float64
		var bytesRead int
		if testOpts.NoDownload {
			log.Info("Download test is disabled")
		} else {
			download, br, err := currentServer.Download(silent, testOpts.Bytes, testOpts.BinaryBase, testOpts.Concurrent, testOpts.Chunks, time.Duration(testOpts.Duration)*time.Second)
			if err != nil {
				log.Errorf("Failed to get download speed: %s", err)
				return err
			}
			downloadValue = download
			bytesRead = br
		}

		// get upload value
		var uploadValue float64
		var bytesWritten int
		if testOpts.NoUpload {
			log.Info("Upload test is disabled")
		} else {
			upload, bw, err := currentServer.Upload(testOpts.NoPreAllocate, silent, testOpts.Bytes, testOpts.BinaryBase, testOpts.Concurrent, testOpts.Chunks, time.Duration(testOpts.Duration)*time.Second)
			if err != nil {
				log.Errorf("Failed to get upload speed: %s", err)
				return err
			}
			uploadValue = upload
			bytesWritten = bw
		}

		// print result if --simple is given
		if c.Bool(defs.OptionSimple) {
			if testOpts.Bytes {
				binaryBase := testOpts.BinaryBase
				log.Warnf("Ping:\t%.2f ms\tJitter:\t%.2f ms\nDownload rate:\t%s\nUpload rate:\t%s", p, jitter, humanizeMbps(downloadValue, binaryBase), humanizeMbps(uploadValue, binaryBase))
			} else {
				log.Warnf("Ping:\t%.2f ms\tJitter:\t%.2f ms\nDownload rate:\t%.2f Mbps\nUpload rate:\t%.2f Mbps", p, jitter, downloadValue, uploadValue)
			}
		}

		// print share link if --share is given
		var shareLink string
		if testOpts.TelemetryServer.GetLevel() > 0 {
			var extra defs.TelemetryExtra
			extra.ServerName = currentServer.Name
			extra.Extra = testOpts.TelemetryExtra

			if link, err := sendTelemetry(testOpts.TelemetryServer, ispInfo, downloadValue, uploadValue, p, jitter, currentServer.TLog.String(), extra); err != nil {
				log.Errorf("Error when sending telemetry data: %s", err)
			} else {
				shareLink = link
				// only print to stdout when --json and --csv are not used
				if !c.Bool(defs.OptionJSON) && !c.Bool(defs.OptionCSV) {
					log.Warnf("Share your result: %s", link)
				}
			}
		}

		var rep report.Report
		rep.Timestamp = time.Now()

		rep.Ping = math.Round(p*100) / 100
		rep.Jitter = math.Round(jitter*100) / 100
		rep.Download = math.Round(downloadValue*100) / 100
		rep.Upload = math.Round(uploadValue*100) / 100
		rep.BytesReceived = bytesRead
		rep.BytesSent = bytesWritten
		rep.Share = shareLink

		rep.Server.Name = currentServer.Name
		rep.Server.URL = u.String()

		rep.Client = report.Client{IPInfoResponse: ispInfo.RawISPInfo}
		rep.Client.Readme = ""

		reps = append(reps, rep)

		//add a new line after each test if testing multiple servers
		if len(testOpts.ServerList) > 1 && !silent {
			log.Warn()
		}
	}

	// check for --csv or --json. the program prioritize the --csv before the --json. this is the same behavior as speedtest-cli
	if c.Bool(defs.OptionCSV) {
		var reps_csv []report.FlatReport
		for _, rep := range reps {
			reps_csv = append(reps_csv, rep.GetFlatReport())
		}
		if resultStrig, err := gocsv.MarshalStringWithoutHeaders(&reps_csv); err != nil {
			log.Errorf("Error generating CSV report: %s", err)
		} else {
			fmt.Print(resultStrig)
		}
	} else if c.Bool(defs.OptionJSON) {
		if jsonBytes, err := json.Marshal(&reps); err != nil {
			log.Errorf("Error generating JSON report: %s", err)
		} else {
			fmt.Println(string(jsonBytes))
		}
	}

	return nil
}

func SpeedTest(testOpts *defs.TestOptions) (*[]report.Report, error) {
	if serverCount := len(testOpts.ServerList); serverCount > 1 {
		log.Debugf("Testing against %d servers", serverCount)
	}

	var reps []report.Report

	// fetch current user's IP info
	for _, currentServer := range testOpts.ServerList {
		// get telemetry level
		currentServer.TLog.SetLevel(testOpts.TelemetryServer.GetLevel())

		u, err := currentServer.GetURL()
		if err != nil {
			log.Errorf("Failed to get server URL: %s", err)
			return nil, err
		}

		log.Debugf("Selected server: %s [%s]", currentServer.Name, u.Hostname())

		if sponsorMsg := currentServer.Sponsor(); sponsorMsg != "" {
			log.Debugf("Sponsored by: %s", sponsorMsg)
		}

		if !currentServer.IsUp() {
			log.Debugf("Selected server %s (%s) is not responding at the moment, try again later", currentServer.Name, u.Hostname())
		}
		ispInfo, err := currentServer.GetIPInfo(string(testOpts.DistanceUnit))
		if err != nil {
			log.Errorf("Failed to get IP info: %s", err)
			return nil, err
		}
		log.Debugf("You're testing from: %s", ispInfo.ProcessedString)

		p, jitter, err := currentServer.ICMPPingAndJitter(pingCount, testOpts.SourceIP, testOpts.Network)
		if err != nil {
			log.Errorf("Failed to get ping and jitter: %s", err)
			return nil, err
		}

		// get download value
		var downloadValue float64
		var bytesRead int
		if testOpts.NoDownload {
			log.Debugf("Download test is disabled")
		} else {
			download, br, err := currentServer.Download(true, testOpts.Bytes, testOpts.BinaryBase, testOpts.Concurrent, testOpts.Chunks, time.Duration(testOpts.Duration)*time.Second)
			if err != nil {
				log.Errorf("Failed to get download speed: %s", err)
				return nil, err
			}
			downloadValue = download
			bytesRead = br
		}

		// get upload value
		var uploadValue float64
		var bytesWritten int
		if testOpts.NoUpload {
			log.Debugf("Upload test is disabled")
		} else {
			upload, bw, err := currentServer.Upload(testOpts.NoPreAllocate, true, testOpts.Bytes, testOpts.BinaryBase, testOpts.Concurrent, testOpts.Chunks, time.Duration(testOpts.Duration)*time.Second)
			if err != nil {
				log.Errorf("Failed to get upload speed: %s", err)
				return nil, err
			}
			uploadValue = upload
			bytesWritten = bw
		}

		// print share link if --share is given
		var shareLink string
		if testOpts.TelemetryServer.GetLevel() > 0 {
			var extra defs.TelemetryExtra
			extra.ServerName = currentServer.Name
			extra.Extra = testOpts.TelemetryExtra

			if link, err := sendTelemetry(testOpts.TelemetryServer, ispInfo, downloadValue, uploadValue, p, jitter, currentServer.TLog.String(), extra); err != nil {
				log.Errorf("Error when sending telemetry data: %s", err)
			} else {
				shareLink = link
			}
		}

		var rep report.Report
		rep.Timestamp = time.Now()

		rep.Ping = math.Round(p*100) / 100
		rep.Jitter = math.Round(jitter*100) / 100
		rep.Download = math.Round(downloadValue*100) / 100
		rep.Upload = math.Round(uploadValue*100) / 100
		rep.BytesReceived = bytesRead
		rep.BytesSent = bytesWritten
		rep.Share = shareLink

		rep.Server.Name = currentServer.Name
		rep.Server.URL = u.String()

		rep.Client = report.Client{IPInfoResponse: ispInfo.RawISPInfo}
		rep.Client.Readme = ""

		reps = append(reps, rep)
	}

	return &reps, nil
}

// sendTelemetry sends the telemetry result to server, if --share is given
func sendTelemetry(telemetryServer defs.TelemetryServer, ispInfo *defs.GetIPResult, download, upload, pingVal, jitter float64, logs string, extra defs.TelemetryExtra) (string, error) {
	var buf bytes.Buffer
	wr := multipart.NewWriter(&buf)

	b, _ := json.Marshal(ispInfo)
	if fIspInfo, err := wr.CreateFormField("ispinfo"); err != nil {
		log.Debugf("Error creating form field: %s", err)
		return "", err
	} else if _, err = fIspInfo.Write(b); err != nil {
		log.Debugf("Error writing form field: %s", err)
		return "", err
	}

	if fDownload, err := wr.CreateFormField("dl"); err != nil {
		log.Debugf("Error creating form field: %s", err)
		return "", err
	} else if _, err = fDownload.Write([]byte(strconv.FormatFloat(download, 'f', 2, 64))); err != nil {
		log.Debugf("Error writing form field: %s", err)
		return "", err
	}

	if fUpload, err := wr.CreateFormField("ul"); err != nil {
		log.Debugf("Error creating form field: %s", err)
		return "", err
	} else if _, err = fUpload.Write([]byte(strconv.FormatFloat(upload, 'f', 2, 64))); err != nil {
		log.Debugf("Error writing form field: %s", err)
		return "", err
	}

	if fPing, err := wr.CreateFormField("ping"); err != nil {
		log.Debugf("Error creating form field: %s", err)
		return "", err
	} else if _, err = fPing.Write([]byte(strconv.FormatFloat(pingVal, 'f', 2, 64))); err != nil {
		log.Debugf("Error writing form field: %s", err)
		return "", err
	}

	if fJitter, err := wr.CreateFormField("jitter"); err != nil {
		log.Debugf("Error creating form field: %s", err)
		return "", err
	} else if _, err = fJitter.Write([]byte(strconv.FormatFloat(jitter, 'f', 2, 64))); err != nil {
		log.Debugf("Error writing form field: %s", err)
		return "", err
	}

	if fLog, err := wr.CreateFormField("log"); err != nil {
		log.Debugf("Error creating form field: %s", err)
		return "", err
	} else if _, err = fLog.Write([]byte(logs)); err != nil {
		log.Debugf("Error writing form field: %s", err)
		return "", err
	}

	b, _ = json.Marshal(extra)
	if fExtra, err := wr.CreateFormField("extra"); err != nil {
		log.Debugf("Error creating form field: %s", err)
		return "", err
	} else if _, err = fExtra.Write(b); err != nil {
		log.Debugf("Error writing form field: %s", err)
		return "", err
	}

	if err := wr.Close(); err != nil {
		log.Debugf("Error flushing form field writer: %s", err)
		return "", err
	}

	telemetryUrl, err := telemetryServer.GetPath()
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, telemetryUrl.String(), &buf)
	if err != nil {
		log.Debugf("Error when creating HTTP request: %s", err)
		return "", err
	}
	req.Header.Set("Content-Type", wr.FormDataContentType())
	req.Header.Set("User-Agent", defs.UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debugf("Error when making HTTP request: %s", err)
		return "", err
	}
	defer resp.Body.Close()

	id, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Errorf("Error when reading HTTP request: %s", err)
		return "", err
	}

	resultUrl, err := telemetryServer.GetShare()
	if err != nil {
		return "", err
	}

	if str := strings.Split(string(id), " "); len(str) != 2 {
		return "", fmt.Errorf("server returned invalid response: %s", id)
	} else {
		q := resultUrl.Query()
		q.Set("id", str[1])
		resultUrl.RawQuery = q.Encode()

		return resultUrl.String(), nil
	}
}

func humanizeMbps(mbps float64, useMebi bool) string {
	val := mbps / 8
	var base float64 = 1000
	if useMebi {
		base = 1024
	}

	if val < 1 {
		if kb := val * base; kb < 1 {
			return fmt.Sprintf("%.2f bytes/s", kb*base)
		} else {
			return fmt.Sprintf("%.2f KB/s", kb)
		}
	} else if val > base {
		return fmt.Sprintf("%.2f GB/s", val/base)
	} else {
		return fmt.Sprintf("%.2f MB/s", val)
	}
}
