package httpclient

import (
	"bytes"
	"context"
	"strings"

	"github.com/minio/sha256-simd"

	//#nosec G505 -- sha1 is supported by Artifactory.
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	ioutils "github.com/jfrog/jfrog-client-go/utils/io"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

type HttpClient struct {
	client             *http.Client
	ctx                context.Context
	retries            int
	retryWaitMilliSecs int
}

const (
	apiKeyPrefix        = "AKCp8"
	apiKeyMinimalLength = 73
	uberTraceIdHeader   = "uber-trace-id"
)

// If set, the Uber Trace ID header will be attached to every request.
// This allows users to easily identify which logs on the server side are related to requests sent from this client.
// Should be set using SetUberTraceIdToken.
var uberTraceIdToken string

func IsApiKey(key string) bool {
	return strings.HasPrefix(key, apiKeyPrefix) && len(key) >= apiKeyMinimalLength
}

func (jc *HttpClient) GetRetries() int {
	return jc.retries
}

func (jc *HttpClient) GetClient() *http.Client {
	return jc.client
}

func (jc *HttpClient) GetRetryWaitTime() int {
	return jc.retryWaitMilliSecs
}

func (jc *HttpClient) sendGetLeaveBodyOpen(url string, followRedirect bool, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, respBody []byte, redirectUrl string, err error) {
	return jc.Send("GET", url, nil, followRedirect, false, httpClientsDetails, logMsgPrefix)
}

func (jc *HttpClient) SendPostLeaveBodyOpen(url string, content []byte, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, err error) {
	resp, _, _, err = jc.Send("POST", url, content, true, false, httpClientsDetails, logMsgPrefix)
	return
}

func (jc *HttpClient) sendGetForFileDownload(url string, followRedirect bool, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, redirectUrl string, err error) {
	resp, _, redirectUrl, err = jc.sendGetLeaveBodyOpen(url, followRedirect, httpClientsDetails, logMsgPrefix)
	return
}

func (jc *HttpClient) Stream(url string, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (*http.Response, []byte, string, error) {
	return jc.sendGetLeaveBodyOpen(url, true, httpClientsDetails, logMsgPrefix)
}

func (jc *HttpClient) SendGet(url string, followRedirect bool, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, respBody []byte, redirectUrl string, err error) {
	return jc.Send("GET", url, nil, followRedirect, true, httpClientsDetails, logMsgPrefix)
}

func (jc *HttpClient) SendPost(url string, content []byte, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, body []byte, err error) {
	resp, body, _, err = jc.Send("POST", url, content, true, true, httpClientsDetails, logMsgPrefix)
	return
}

func (jc *HttpClient) SendPatch(url string, content []byte, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, body []byte, err error) {
	resp, body, _, err = jc.Send("PATCH", url, content, true, true, httpClientsDetails, logMsgPrefix)
	return
}

func (jc *HttpClient) SendDelete(url string, content []byte, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, body []byte, err error) {
	resp, body, _, err = jc.Send("DELETE", url, content, true, true, httpClientsDetails, logMsgPrefix)
	return
}

func (jc *HttpClient) SendHead(url string, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, body []byte, err error) {
	resp, body, _, err = jc.Send("HEAD", url, nil, true, true, httpClientsDetails, logMsgPrefix)
	return
}

func (jc *HttpClient) SendPut(url string, content []byte, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, body []byte, err error) {
	resp, body, _, err = jc.Send("PUT", url, content, true, true, httpClientsDetails, logMsgPrefix)
	return
}

func (jc *HttpClient) newRequest(method, url string, body io.Reader) (req *http.Request, err error) {
	if jc.ctx != nil {
		req, err = http.NewRequestWithContext(jc.ctx, method, url, body)
	} else {
		req, err = http.NewRequest(method, url, body)
	}
	return req, errorutils.CheckError(err)
}

func (jc *HttpClient) Send(method, url string, content []byte, followRedirect, closeBody bool, httpClientsDetails httputils.HttpClientDetails, logMsgPrefix string) (resp *http.Response, respBody []byte, redirectUrl string, err error) {
	retryExecutor := utils.RetryExecutor{
		Context:                  jc.ctx,
		MaxRetries:               jc.retries,
		RetriesIntervalMilliSecs: jc.retryWaitMilliSecs,
		LogMsgPrefix:             logMsgPrefix,
		ErrorMessage:             fmt.Sprintf("Failure occurred while sending %s request to %s", method, url),
		ExecutionHandler: func() (bool, error) {
			var req *http.Request
			req, err = jc.createReq(method, url, content)
			if err != nil {
				return true, err
			}
			resp, respBody, redirectUrl, err = jc.doRequest(req, content, followRedirect, closeBody, httpClientsDetails)
			if err != nil {
				if strings.Contains(err.Error(), "unsupported protocol scheme") {
					// Wrong URL, so no need to retry
					return false, fmt.Errorf("%w\nThe received error indicates an invalid URL: %q, Please ensure the URL includes a valid scheme like 'http://' or 'https://'.", err, url)
				}
				return true, err
			}
			// Response must not be nil
			if resp == nil {
				return false, errorutils.CheckErrorf("%sReceived empty response from server", logMsgPrefix)
			}
			if !jc.shouldRetry(resp, &httpClientsDetails) {
				return false, nil
			}
			// Perform retry
			log.Warn(fmt.Sprintf("%sThe server response: %s\n%s", logMsgPrefix, resp.Status, utils.IndentJson(respBody)))
			return true, nil
		},
	}

	err = retryExecutor.Execute()
	return
}

func (jc *HttpClient) shouldRetry(resp *http.Response, httpClientsDetails *httputils.HttpClientDetails) bool {
	// If response-code < 500 and it is not 429, should not retry
	if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	// If any of the preretry interceptors is false - return false
	for _, shouldRetry := range httpClientsDetails.PreRetryInterceptors {
		if !shouldRetry() {
			return false
		}
	}
	return true
}

func (jc *HttpClient) createReq(method, url string, content []byte) (req *http.Request, err error) {
	if content != nil {
		return jc.newRequest(method, url, bytes.NewBuffer(content))
	}
	return jc.newRequest(method, url, nil)
}

func (jc *HttpClient) doRequest(req *http.Request, content []byte, followRedirect bool, closeBody bool, httpClientsDetails httputils.HttpClientDetails) (resp *http.Response, respBody []byte, redirectUrl string, err error) {
	log.Debug(fmt.Sprintf("Sending HTTP %s request to: %s", req.Method, req.URL))
	req.Close = true
	setAuthentication(req, httpClientsDetails)
	addUserAgentHeader(req)
	copyHeaders(httpClientsDetails, req)
	addUberTraceIdHeaderIfSet(req)

	client := jc.client

	if !followRedirect || (followRedirect && req.Method == http.MethodPost) {
		// The jc.client is a shared resource between go routines, so to handle this override we clone it.
		client = cloneHttpClient(jc.client)
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			redirectUrl = req.URL.String()
			return errors.New("redirect")
		}
	}

	resp, err = client.Do(req)
	if err != nil && redirectUrl != "" {
		if !followRedirect {
			log.Debug("Blocking HTTP redirect to", redirectUrl)
			return
		}
		// Due to security reasons, there's no built-in HTTP redirect in the HTTP Client
		// for POST requests. We therefore implement the redirect on our own.
		if req.Method == http.MethodPost {
			log.Debug("HTTP redirecting to", redirectUrl)
			resp, respBody, err = jc.SendPost(redirectUrl, content, httpClientsDetails, "")
			redirectUrl = ""
			return
		}
	}
	if errorutils.CheckError(err) != nil {
		return
	}
	if closeBody {
		defer func() {
			if resp != nil && resp.Body != nil {
				err = errors.Join(err, errorutils.CheckError(resp.Body.Close()))
			}
		}()
		respBody, _ = io.ReadAll(resp.Body)
	}
	return
}

func cloneHttpClient(httpClient *http.Client) *http.Client {
	return &http.Client{
		Transport:     httpClient.Transport,
		Timeout:       httpClient.Timeout,
		Jar:           httpClient.Jar,
		CheckRedirect: httpClient.CheckRedirect,
	}
}

func copyHeaders(httpClientsDetails httputils.HttpClientDetails, req *http.Request) {
	if httpClientsDetails.Headers != nil {
		for name := range httpClientsDetails.Headers {
			req.Header.Set(name, httpClientsDetails.Headers[name])
		}
	}
}

// Generate an Uber Trace ID token that will be attached to every request.
// Format of the header: {trace-id}:{span-id}:{parent-span-id}:{flags}
// We set the trace-id and span-id to the same value, and the rest to 0.
func SetUberTraceIdToken(traceIdToken string) {
	uberTraceIdToken = fmt.Sprintf("%s:%s:0:0", traceIdToken, traceIdToken)
}

// If a trace ID is set, this function will attach the Uber Trace ID header to every request.
func addUberTraceIdHeaderIfSet(req *http.Request) {
	if uberTraceIdToken == "" {
		return
	}
	req.Header.Set(uberTraceIdHeader, uberTraceIdToken)
}

func setRequestHeaders(httpClientsDetails httputils.HttpClientDetails, size int64, req *http.Request) {
	copyHeaders(httpClientsDetails, req)
	length := strconv.FormatInt(size, 10)
	req.Header.Set("Content-Length", length)
}

// You may implement the log.Progress interface, or pass nil to run without progress display.
func (jc *HttpClient) UploadFile(localPath, url, logMsgPrefix string, httpClientsDetails httputils.HttpClientDetails,
	progress ioutils.ProgressMgr) (resp *http.Response, body []byte, err error) {
	if progress != nil {
		progress.IncrementGeneralProgress()
	}
	retryExecutor := utils.RetryExecutor{
		MaxRetries:               jc.retries,
		RetriesIntervalMilliSecs: jc.retryWaitMilliSecs,
		ErrorMessage:             fmt.Sprintf("Failure occurred while uploading to %s", url),
		LogMsgPrefix:             logMsgPrefix,
		ExecutionHandler: func() (bool, error) {
			resp, body, err = jc.doUploadFile(localPath, url, httpClientsDetails, progress)
			if err != nil {
				return true, err
			}
			// Response must not be nil
			if resp == nil {
				return false, errorutils.CheckErrorf("%sReceived empty response from file upload", logMsgPrefix)
			}
			// If response-code < 500, should not retry
			if resp.StatusCode < 500 {
				return false, nil
			}
			// Perform retry
			log.Warn(fmt.Sprintf("%sThe server response: %s\n%s", logMsgPrefix, resp.Status, utils.IndentJson(body)))
			return true, nil
		},
	}
	err = retryExecutor.Execute()
	return
}

func (jc *HttpClient) doUploadFile(localPath, url string, httpClientsDetails httputils.HttpClientDetails,
	progress ioutils.ProgressMgr) (resp *http.Response, body []byte, err error) {
	var file *os.File
	if localPath != "" {
		file, err = os.Open(localPath)
		defer func() {
			if file != nil {
				err = errors.Join(err, errorutils.CheckError(file.Close()))
			}
		}()
		if errorutils.CheckError(err) != nil {
			return nil, nil, err
		}
	}

	size, err := fileutils.GetFileSize(file)
	if err != nil {
		return nil, nil, err
	}

	reqContent := fileutils.GetUploadRequestContent(file)
	var reader io.Reader
	if file != nil && progress != nil {
		progressReader := progress.NewProgressReader(size, "Uploading", url)
		reader = progressReader.ActionWithProgress(reqContent)
		progressId := progressReader.GetId()
		defer progress.RemoveProgress(progressId)
	} else {
		reader = reqContent
	}

	resp, body, err = jc.UploadFileFromReader(reader, url, httpClientsDetails, size)
	return
}

func (jc *HttpClient) UploadFileFromReader(reader io.Reader, url string, httpClientsDetails httputils.HttpClientDetails,
	size int64) (resp *http.Response, body []byte, err error) {
	req, err := jc.newRequest(http.MethodPut, url, reader)
	if err != nil {
		return
	}
	req.ContentLength = size
	req.Close = true

	setRequestHeaders(httpClientsDetails, size, req)
	setAuthentication(req, httpClientsDetails)
	addUserAgentHeader(req)

	client := jc.client
	resp, err = client.Do(req)
	if errorutils.CheckError(err) != nil || resp == nil {
		return
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			err = errors.Join(err, errorutils.CheckError(resp.Body.Close()))
		}
	}()
	if err = errorutils.CheckResponseStatus(resp, http.StatusCreated, http.StatusOK, http.StatusAccepted); err != nil {
		return
	}
	body, err = io.ReadAll(resp.Body)
	err = errorutils.CheckError(err)
	return
}

// Read remote file,
// The caller is responsible to check if resp.StatusCode is StatusOK before reading, and to close io.ReadCloser after done reading.
func (jc *HttpClient) ReadRemoteFile(downloadPath string, httpClientsDetails httputils.HttpClientDetails) (io.ReadCloser, *http.Response, error) {
	resp, _, err := jc.sendGetForFileDownload(downloadPath, true, httpClientsDetails, "")
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp, nil
	}
	return resp.Body, resp, nil
}

// Bulk downloads a file.
// You may implement the log.Progress interface, or pass nil to run without progress display.
func (jc *HttpClient) DownloadFileWithProgress(downloadFileDetails *DownloadFileDetails, logMsgPrefix string,
	httpClientsDetails httputils.HttpClientDetails, isExplode, isBypassArchiveInspection bool, progress ioutils.ProgressMgr) (*http.Response, error) {
	resp, _, err := jc.downloadFile(downloadFileDetails, logMsgPrefix, true, httpClientsDetails, isExplode, isBypassArchiveInspection, progress)
	return resp, err
}

// Bulk downloads a file.
func (jc *HttpClient) DownloadFile(downloadFileDetails *DownloadFileDetails, logMsgPrefix string,
	httpClientsDetails httputils.HttpClientDetails, isExplode, bypassArchiveInspection bool) (*http.Response, error) {
	return jc.DownloadFileWithProgress(downloadFileDetails, logMsgPrefix, httpClientsDetails, isExplode, bypassArchiveInspection, nil)
}

func (jc *HttpClient) DownloadFileNoRedirect(downloadPath, localPath, fileName string, httpClientsDetails httputils.HttpClientDetails) (*http.Response, string, error) {
	downloadFileDetails := &DownloadFileDetails{DownloadPath: downloadPath, LocalPath: localPath, FileName: fileName}
	return jc.downloadFile(downloadFileDetails, "", false, httpClientsDetails, false, false, nil)
}

func (jc *HttpClient) downloadFile(downloadFileDetails *DownloadFileDetails, logMsgPrefix string, followRedirect bool,
	httpClientsDetails httputils.HttpClientDetails, isExplode, bypassArchiveInspection bool, progress ioutils.ProgressMgr) (resp *http.Response, redirectUrl string, err error) {
	retryExecutor := utils.RetryExecutor{
		MaxRetries:               jc.retries,
		RetriesIntervalMilliSecs: jc.retryWaitMilliSecs,
		ErrorMessage:             fmt.Sprintf("Failure occurred while downloading %s", downloadFileDetails.DownloadPath),
		LogMsgPrefix:             logMsgPrefix,
		ExecutionHandler: func() (bool, error) {
			resp, redirectUrl, err = jc.doDownloadFile(downloadFileDetails, logMsgPrefix, followRedirect, httpClientsDetails, isExplode, bypassArchiveInspection, progress)
			// In case followRedirect is 'false' and doDownloadFile did redirect, an error is returned and redirectUrl
			// receives the redirect address. This case should not retry.
			if err != nil && !followRedirect && redirectUrl != "" {
				return false, err
			}
			// If error occurred during doDownloadFile, perform retry.
			if err != nil {
				return true, err
			}
			// Response must not be nil
			if resp == nil {
				return false, errorutils.CheckErrorf("%sReceived empty response from file download", logMsgPrefix)
			}
			// If response-code < 500, should not retry
			if resp.StatusCode < 500 {
				return false, nil
			}
			// Perform retry
			log.Warn(fmt.Sprintf("%sThe server response: %s", logMsgPrefix, resp.Status))
			return true, nil
		},
	}

	err = retryExecutor.Execute()
	return
}

func (jc *HttpClient) doDownloadFile(downloadFileDetails *DownloadFileDetails, logMsgPrefix string, followRedirect bool,
	httpClientsDetails httputils.HttpClientDetails, isExplode, bypassArchiveInspection bool, progress ioutils.ProgressMgr) (resp *http.Response, redirectUrl string, err error) {
	resp, redirectUrl, err = jc.sendGetForFileDownload(downloadFileDetails.DownloadPath, followRedirect, httpClientsDetails, "")
	if err != nil {
		return
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			err = errors.Join(err, errorutils.CheckError(resp.Body.Close()))
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return resp, redirectUrl, nil
	}

	// Save the file to the file system.
	err = saveToFile(downloadFileDetails, resp, progress)
	if err != nil {
		return
	}

	// Extract archive.
	if isExplode {
		err = utils.ExtractArchive(downloadFileDetails.LocalPath, downloadFileDetails.LocalFileName, downloadFileDetails.FileName, logMsgPrefix, bypassArchiveInspection)
	}
	return
}

func saveToFile(downloadFileDetails *DownloadFileDetails, resp *http.Response, progress ioutils.ProgressMgr) (err error) {
	fileName, err := fileutils.CreateFilePath(downloadFileDetails.LocalPath, downloadFileDetails.LocalFileName)
	if err != nil {
		return err
	}

	out, err := os.Create(fileName)
	if errorutils.CheckError(err) != nil {
		return
	}

	defer func() {
		err = errors.Join(err, errorutils.CheckError(out.Close()))
	}()

	var reader io.Reader
	if progress != nil {
		progressReader := progress.NewProgressReader(resp.ContentLength, "", downloadFileDetails.RelativePath)
		reader = progressReader.ActionWithProgress(resp.Body)
		progressId := progressReader.GetId()
		defer progress.RemoveProgress(progressId)
	} else {
		reader = resp.Body
	}

	expectedSha, actualSha := handleExpectedSha(downloadFileDetails.ExpectedSha1, downloadFileDetails.ExpectedSha256)
	if len(expectedSha) > 0 && !downloadFileDetails.SkipChecksum {
		writer := io.MultiWriter(actualSha, out)

		_, err = io.Copy(writer, reader)
		if errorutils.CheckError(err) != nil {
			return err
		}

		err = validateChecksum(expectedSha, actualSha, downloadFileDetails.LocalFileName)
	} else {
		_, err = io.Copy(out, reader)
	}

	return errorutils.CheckError(err)
}

// Downloads a file by chunks, concurrently.
// If successful, returns the resp of the last chunk, which will have resp.StatusCode = http.StatusPartialContent
// Otherwise: if an error occurred - returns the error with resp=nil, else - err=nil and the resp of the first chunk that received statusCode!=http.StatusPartialContent
// The caller is responsible to check the resp.StatusCode.
// You may implement the log.Progress interface, or pass nil to run without progress display.
func (jc *HttpClient) DownloadFileConcurrently(flags ConcurrentDownloadFlags, logMsgPrefix string,
	httpClientsDetails httputils.HttpClientDetails, progress ioutils.ProgressMgr) (resp *http.Response, err error) {
	// Create temp dir for file chunks.
	tempDirPath, err := fileutils.CreateTempDir()
	if err != nil {
		return
	}
	defer func() {
		err = errors.Join(err, fileutils.RemoveTempDir(tempDirPath))
	}()

	chunksPaths := make([]string, flags.SplitCount)

	var downloadProgressId int
	if progress != nil {
		downloadProgress := progress.NewProgressReader(flags.FileSize, "", flags.RelativePath)
		downloadProgressId = downloadProgress.GetId()
		// Aborting order matters. mergingProgress depends on the existence of downloadingProgress
		defer progress.RemoveProgress(downloadProgressId)
	}

	resp, err = jc.downloadChunksConcurrently(chunksPaths, flags, logMsgPrefix, tempDirPath, httpClientsDetails, progress, downloadProgressId)
	if err != nil {
		return
	}
	// If not all chunks were downloaded successfully, return
	if resp.StatusCode != http.StatusPartialContent {
		return
	}

	if flags.LocalPath != "" {
		err = os.MkdirAll(flags.LocalPath, 0777)
		if errorutils.CheckError(err) != nil {
			return
		}
		flags.LocalFileName = filepath.Join(flags.LocalPath, flags.LocalFileName)
	}

	if fileutils.IsPathExists(flags.LocalFileName, false) {
		err = os.Remove(flags.LocalFileName)
		if errorutils.CheckError(err) != nil {
			return
		}
	}
	if progress != nil {
		progress.SetMergingState(downloadProgressId, true)
	}
	err = mergeChunks(chunksPaths, flags)
	if errorutils.CheckError(err) != nil {
		return
	}

	if flags.Explode {
		if err = utils.ExtractArchive(flags.LocalPath, flags.LocalFileName, flags.FileName, logMsgPrefix, flags.BypassArchiveInspection); err != nil {
			return
		}
	}

	log.Info(logMsgPrefix + "Done downloading.")
	return
}

// The caller is responsible to check that resp.StatusCode is http.StatusOK
func (jc *HttpClient) GetRemoteFileDetails(downloadUrl string, httpClientsDetails httputils.HttpClientDetails) (*fileutils.FileDetails, *http.Response, error) {
	resp, body, err := jc.SendHead(downloadUrl, httpClientsDetails, "")
	if err != nil {
		return nil, nil, err
	}

	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
		return nil, nil, err
	}
	log.Debug("Artifactory response:", resp.Status)

	fileSize := int64(0)
	contentLength := resp.Header.Get("Content-Length")
	if len(contentLength) > 0 {
		fileSize, err = strconv.ParseInt(contentLength, 10, 64)
		if errorutils.CheckError(err) != nil {
			return nil, nil, err
		}
	}

	fileDetails := new(fileutils.FileDetails)
	fileDetails.Checksum.Md5 = resp.Header.Get("X-Checksum-Md5")
	fileDetails.Checksum.Sha1 = resp.Header.Get("X-Checksum-Sha1")
	fileDetails.Checksum.Sha256 = resp.Header.Get("X-Checksum-Sha256")
	fileDetails.Size = fileSize
	return fileDetails, resp, nil
}

// Downloads chunks, concurrently.
// If successful, returns the resp of the last chunk, which will have resp.StatusCode = http.StatusPartialContent
// Otherwise: if an error occurred - returns the error with resp=nil, else - err=nil and the resp of the first chunk that received statusCode!=http.StatusPartialContent
// The caller is responsible to check the resp.StatusCode.
func (jc *HttpClient) downloadChunksConcurrently(chunksPaths []string, flags ConcurrentDownloadFlags, logMsgPrefix,
	chunksDownloadPath string, httpClientsDetails httputils.HttpClientDetails, progress ioutils.ProgressMgr, progressId int) (*http.Response, error) {
	var wg sync.WaitGroup
	chunkSize := flags.FileSize / int64(flags.SplitCount)
	mod := flags.FileSize % int64(flags.SplitCount)
	// Create a list of errors, to allow each go routine to save there its own returned error.
	errorsList := make([]error, flags.SplitCount)
	// Store the responses, to return a response with unexpected statusCode or the last response if all successful
	respList := make([]*http.Response, flags.SplitCount)
	// Global vars on top of the go routines, to break the loop earlier if needed
	var err error
	var resp *http.Response
	for i := 0; i < flags.SplitCount; i++ {
		// Checking this global error may help break out of the loop earlier, if an error or the wrong status code was received
		// has already been returned by one of the go routines.
		if err != nil {
			break
		}
		if resp != nil && resp.StatusCode != http.StatusPartialContent {
			break
		}
		wg.Add(1)
		start := chunkSize * int64(i)
		end := chunkSize * (int64(i) + 1)
		if i == flags.SplitCount-1 {
			end += mod
		}
		requestClientDetails := httpClientsDetails.Clone()
		go func(start, end int64, i int) {
			chunksPaths[i], respList[i], errorsList[i] = jc.downloadFileRange(flags, start, end, i, logMsgPrefix, chunksDownloadPath, *requestClientDetails, progress, progressId)
			// Write to the global vars if the chunk wasn't downloaded successfully
			if errorsList[i] != nil {
				err = errorsList[i]
			}
			if respList[i] != nil && respList[i].StatusCode != http.StatusPartialContent {
				resp = respList[i]
			}
			wg.Done()
		}(start, end, i)
	}
	wg.Wait()

	// Verify that all chunks have been downloaded successfully.
	for _, e := range errorsList {
		if e != nil {
			return nil, errorutils.CheckError(e)
		}
	}
	for _, r := range respList {
		if r.StatusCode != http.StatusPartialContent {
			return r, nil
		}
	}

	// If all chunks were downloaded successfully, return the response of the last chunk.
	return respList[len(respList)-1], nil
}

func mergeChunks(chunksPaths []string, flags ConcurrentDownloadFlags) (err error) {
	destFile, err := os.OpenFile(flags.LocalFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if errorutils.CheckError(err) != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, errorutils.CheckError(destFile.Close()))
	}()
	var writer io.Writer
	expectedSha, actualSha := handleExpectedSha(flags.ExpectedSha1, flags.ExpectedSha256)
	if len(expectedSha) > 0 {
		writer = io.MultiWriter(actualSha, destFile)
	} else {
		writer = io.MultiWriter(destFile)
	}
	for i := 0; i < flags.SplitCount; i++ {
		// Wrapping the loop body in an anonymous function to ensure deferred calls
		// are executed at the end of each iteration, not at the end of the enclosing function.
		err = func() (e error) {
			reader, e := os.Open(chunksPaths[i])
			if errorutils.CheckError(e) != nil {
				return e
			}
			defer func() {
				e = errors.Join(e, errorutils.CheckError(reader.Close()))
			}()

			_, e = io.Copy(writer, reader)
			if errorutils.CheckError(e) != nil {
				return e
			}

			return nil
		}()
		if err != nil {
			return err
		}
	}
	if len(expectedSha) > 0 && !flags.SkipChecksum {
		err = validateChecksum(expectedSha, actualSha, flags.LocalFileName)
	}
	return err
}
func validateChecksum(expectedSha string, actualSha hash.Hash, fileName string) (err error) {
	actualShaString := hex.EncodeToString(actualSha.Sum(nil))
	if actualShaString != expectedSha {
		err = errorutils.CheckErrorf("checksum mismatch for  " + fileName + ", expected: " + expectedSha + ", actual: " + actualShaString)
	}
	return
}

func handleExpectedSha(expectedSha1, expectedSha256 string) (expectedSha string, actualSha hash.Hash) {
	if len(expectedSha1) > 0 {
		expectedSha = expectedSha1
		//#nosec G401 jfrog-ignore -- Sha1 is supported by Artifactory.
		actualSha = sha1.New()
	} else if len(expectedSha256) > 0 {
		expectedSha = expectedSha256
		actualSha = sha256.New()
	}
	return
}

func (jc *HttpClient) downloadFileRange(flags ConcurrentDownloadFlags, start, end int64, currentSplit int, logMsgPrefix, chunkDownloadPath string,
	httpClientsDetails httputils.HttpClientDetails, progress ioutils.ProgressMgr, progressId int) (fileName string, resp *http.Response, err error) {
	retryExecutor := utils.RetryExecutor{
		MaxRetries:               jc.retries,
		RetriesIntervalMilliSecs: jc.retryWaitMilliSecs,
		ErrorMessage:             fmt.Sprintf("Failure occurred while downloading part %d of %s", currentSplit, flags.DownloadPath),
		LogMsgPrefix:             fmt.Sprintf("%s[%s]: ", logMsgPrefix, strconv.Itoa(currentSplit)),
		ExecutionHandler: func() (bool, error) {
			fileName, resp, err = jc.doDownloadFileRange(flags, start, end, currentSplit, logMsgPrefix, chunkDownloadPath, httpClientsDetails, progress, progressId)
			if err != nil {
				return true, err
			}
			// Response must not be nil
			if resp == nil {
				return false, errorutils.CheckErrorf("%s[%s]: Received empty response from file download", logMsgPrefix, strconv.Itoa(currentSplit))
			}
			// If response-code < 500, should not retry
			if resp.StatusCode < 500 {
				return false, nil
			}
			// Perform retry
			log.Warn(fmt.Sprintf("%s[%s]: The server response: %s", logMsgPrefix, strconv.Itoa(currentSplit), resp.Status))
			return true, nil
		},
	}

	err = retryExecutor.Execute()
	return
}

func (jc *HttpClient) doDownloadFileRange(flags ConcurrentDownloadFlags, start, end int64, currentSplit int, logMsgPrefix, chunkDownloadPath string,
	httpClientsDetails httputils.HttpClientDetails, progress ioutils.ProgressMgr, progressId int) (fileName string, resp *http.Response, err error) {

	tempFile, err := os.CreateTemp(chunkDownloadPath, strconv.Itoa(currentSplit)+"_")
	if errorutils.CheckError(err) != nil {
		return
	}
	defer func() {
		err = errors.Join(err, errorutils.CheckError(tempFile.Close()))
	}()

	if httpClientsDetails.Headers == nil {
		httpClientsDetails.Headers = make(map[string]string)
	}
	httpClientsDetails.Headers["Range"] = "bytes=" + strconv.FormatInt(start, 10) + "-" + strconv.FormatInt(end-1, 10)
	resp, _, err = jc.sendGetForFileDownload(flags.DownloadPath, true, httpClientsDetails, "")
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			err = errors.Join(err, errorutils.CheckError(resp.Body.Close()))
		}
	}()
	// Unexpected http response
	if resp.StatusCode != http.StatusPartialContent {
		return
	}
	log.Info(fmt.Sprintf("%s[%s]: %s...", logMsgPrefix, strconv.Itoa(currentSplit), resp.Status))

	err = os.MkdirAll(chunkDownloadPath, 0777)
	if errorutils.CheckError(err) != nil {
		return "", nil, err
	}

	var reader io.Reader
	if progress != nil {
		reader = progress.GetProgress(progressId).ActionWithProgress(resp.Body)
	} else {
		reader = resp.Body
	}

	_, err = io.Copy(tempFile, reader)

	if errorutils.CheckError(err) != nil {
		return "", nil, err
	}
	return tempFile.Name(), resp, errorutils.CheckError(err)
}

// The caller is responsible to check if resp.StatusCode is StatusOK before relying on the bool value
func (jc *HttpClient) IsAcceptRanges(downloadUrl string, httpClientsDetails httputils.HttpClientDetails) (bool, *http.Response, error) {
	resp, _, err := jc.SendHead(downloadUrl, httpClientsDetails, "")
	if errorutils.CheckError(err) != nil {
		return false, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, resp, nil
	}
	return resp.Header.Get("Accept-Ranges") == "bytes", resp, nil
}

func setAuthentication(req *http.Request, httpClientsDetails httputils.HttpClientDetails) {
	// Set authentication
	if httpClientsDetails.ApiKey != "" {
		if httpClientsDetails.User != "" {
			req.SetBasicAuth(httpClientsDetails.User, httpClientsDetails.ApiKey)
		} else {
			req.Header.Set("X-JFrog-Art-Api", httpClientsDetails.ApiKey)
		}
		return
	}
	if httpClientsDetails.AccessToken != "" {
		if IsApiKey(httpClientsDetails.AccessToken) {
			log.Warn("The provided Access Token is an API key and will be used as a password in username/password authentication.\n" +
				"To avoid this message in the future please use it as a password.")
			req.SetBasicAuth(httpClientsDetails.User, httpClientsDetails.AccessToken)
		} else {
			req.Header.Set("Authorization", "Bearer "+httpClientsDetails.AccessToken)
		}
		return
	}
	if httpClientsDetails.Password != "" {
		req.SetBasicAuth(httpClientsDetails.User, httpClientsDetails.Password)
	}
}

func addUserAgentHeader(req *http.Request) {
	req.Header.Set("User-Agent", utils.GetUserAgent())
}

type DownloadFileDetails struct {
	FileName       string `json:"FileName,omitempty"`
	DownloadPath   string `json:"DownloadPath,omitempty"`
	RelativePath   string `json:"RelativePath,omitempty"`
	LocalPath      string `json:"LocalPath,omitempty"`
	LocalFileName  string `json:"LocalFileName,omitempty"`
	ExpectedSha1   string `json:"ExpectedSha1,omitempty"`
	ExpectedSha256 string `json:"-"`
	Size           int64  `json:"Size,omitempty"`
	SkipChecksum   bool   `json:"SkipChecksum,omitempty"`
}

type ConcurrentDownloadFlags struct {
	FileName                string
	DownloadPath            string
	RelativePath            string
	LocalFileName           string
	LocalPath               string
	ExpectedSha1            string
	ExpectedSha256          string
	FileSize                int64
	SplitCount              int
	Explode                 bool
	BypassArchiveInspection bool
	SkipChecksum            bool
}
