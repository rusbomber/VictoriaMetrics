package syslog

import (
	"bufio"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/gzip"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vlinsert/insertutils"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vlstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/cgroup"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/ingestserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/netutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/protoparser/common"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/slicesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/writeconcurrencylimiter"
	"github.com/VictoriaMetrics/metrics"
)

var (
	syslogTenantID = flag.String("syslog.tenantID", "0:0", "TenantID for logs ingested via Syslog protocol. See https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")
	syslogTimezone = flag.String("syslog.timezone", "Local", "Timezone to use when parsing timestamps in RFC3164 syslog messages. Timezone must be a valid IANA Time Zone. "+
		"For example: America/New_York, Europe/Berlin, Etc/GMT+3 . See https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")

	listenAddrTCP = flag.String("syslog.listenAddr.tcp", "", "Optional TCP address to listen to for Syslog messages. See https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")
	listenAddrUDP = flag.String("syslog.listenAddr.udp", "", "Optional UDP address to listen to for Syslog messages. See https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")

	tlsEnable = flag.Bool("syslog.tls", false, "Whether to use TLS for receiving syslog messages at -syslog.listenAddr.tcp. -syslog.tlsCertFile and -syslog.tlsKeyFile must be set "+
		"if -syslog.tls is set. See https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")
	tlsCertFile = flag.String("syslog.tlsCertFile", "", "Path to file with TLS certificate for -syslog.listenAddr.tcp if -syslog.tls is set. "+
		"Prefer ECDSA certs instead of RSA certs as RSA certs are slower. The provided certificate file is automatically re-read every second, so it can be dynamically updated. "+
		"See https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")
	tlsKeyFile = flag.String("syslog.tlsKeyFile", "", "Path to file with TLS key for  -syslog.listenAddr.tcp if -syslog.tls is set. "+
		"The provided key file is automatically re-read every second, so it can be dynamically updated")
	tlsCipherSuites = flagutil.NewArrayString("syslog.tlsCipherSuites", "Optional list of TLS cipher suites for -syslog.listenAddr.tcp if -syslog.tls is set. "+
		"See the list of supported cipher suites at https://pkg.go.dev/crypto/tls#pkg-constants . "+
		"See also https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")
	tlsMinVersion = flag.String("syslog.tlsMinVersion", "TLS13", "The minimum TLS version to use for -syslog.listenAddr.tcp if -syslog.tls is set. "+
		"Supported values: TLS10, TLS11, TLS12, TLS13. "+
		"See https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")

	compressMethod = flag.String("syslog.compressMethod", "", "Compression method for syslog messages received at -syslog.listenAddr.tcp and -syslog.listenAddr.udp. "+
		"Supported values: none, gzip, deflate. See https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/")
)

// MustInit initializes syslog parser at the given -syslog.listenAddr.tcp and -syslog.listenAddr.udp ports
//
// This function must be called after flag.Parse().
//
// MustStop() must be called in order to free up resources occupied by the initialized syslog parser.
func MustInit() {
	if workersStopCh != nil {
		logger.Panicf("BUG: MustInit() called twice without MustStop() call")
	}
	workersStopCh = make(chan struct{})

	tenantID, err := logstorage.GetTenantIDFromString(*syslogTenantID)
	if err != nil {
		logger.Fatalf("cannot parse -syslog.tenantID=%q: %s", *syslogTenantID, err)
	}
	globalTenantID = tenantID

	switch *compressMethod {
	case "", "none", "gzip", "deflate":
	default:
		logger.Fatalf("unexpected -syslog.compressLevel=%q; supported values: none, gzip, deflate", *compressMethod)
	}

	if *listenAddrTCP != "" {
		workersWG.Add(1)
		go func() {
			runTCPListener(*listenAddrTCP)
			workersWG.Done()
		}()
	}

	if *listenAddrUDP != "" {
		workersWG.Add(1)
		go func() {
			runUDPListener(*listenAddrUDP)
			workersWG.Done()
		}()
	}

	currentYear := time.Now().Year()
	globalCurrentYear.Store(int64(currentYear))
	workersWG.Add(1)
	go func() {
		ticker := time.NewTicker(time.Minute)
		for {
			select {
			case <-workersStopCh:
				ticker.Stop()
				workersWG.Done()
				return
			case <-ticker.C:
				currentYear := time.Now().Year()
				globalCurrentYear.Store(int64(currentYear))
			}
		}
	}()

	if *syslogTimezone != "" {
		tz, err := time.LoadLocation(*syslogTimezone)
		if err != nil {
			logger.Fatalf("cannot parse -syslog.timezone=%q: %s", *syslogTimezone, err)
		}
		globalTimezone = tz
	} else {
		globalTimezone = time.Local
	}
}

var (
	globalTenantID    logstorage.TenantID
	globalCurrentYear atomic.Int64
	globalTimezone    *time.Location
)

var (
	workersWG     sync.WaitGroup
	workersStopCh chan struct{}
)

// MustStop stops syslog parser initialized via MustInit()
func MustStop() {
	close(workersStopCh)
	workersWG.Wait()
	workersStopCh = nil
}

func runUDPListener(addr string) {
	ln, err := net.ListenPacket(netutil.GetUDPNetwork(), addr)
	if err != nil {
		logger.Fatalf("cannot start UDP syslog server at %q: %s", addr, err)
	}

	doneCh := make(chan struct{})
	go func() {
		serveUDP(ln)
		close(doneCh)
	}()

	<-workersStopCh
	if err := ln.Close(); err != nil {
		logger.Fatalf("syslog: cannot close UDP listener at %s: %s", addr, err)
	}
	<-doneCh
}

func runTCPListener(addr string) {
	var tlsConfig *tls.Config
	if *tlsEnable {
		tc, err := netutil.GetServerTLSConfig(*tlsCertFile, *tlsKeyFile, *tlsMinVersion, *tlsCipherSuites)
		if err != nil {
			logger.Fatalf("cannot load TLS cert from -syslog.tlsCertFile=%q, -syslog.tlsKeyFile=%q, -syslog.tlsMinVersion=%q, -syslog.tlsCipherSuites=%q: %s",
				*tlsCertFile, *tlsKeyFile, *tlsMinVersion, *tlsCipherSuites, err)
		}
		tlsConfig = tc
	}
	ln, err := netutil.NewTCPListener("syslog", addr, false, tlsConfig)
	if err != nil {
		logger.Fatalf("syslog: cannot start TCP listener at %s: %s", addr, err)
	}

	doneCh := make(chan struct{})
	go func() {
		serveTCP(ln)
		close(doneCh)
	}()

	<-workersStopCh
	if err := ln.Close(); err != nil {
		logger.Fatalf("syslog: cannot close TCP listener at %s: %s", addr, err)
	}
	<-doneCh
}

func serveUDP(ln net.PacketConn) {
	gomaxprocs := cgroup.AvailableCPUs()
	var wg sync.WaitGroup
	localAddr := ln.LocalAddr()
	for i := 0; i < gomaxprocs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cp := insertutils.GetCommonParamsForSyslog(globalTenantID)
			var bb bytesutil.ByteBuffer
			bb.B = bytesutil.ResizeNoCopyNoOverallocate(bb.B, 64*1024)
			for {
				bb.Reset()
				bb.B = bb.B[:cap(bb.B)]
				n, remoteAddr, err := ln.ReadFrom(bb.B)
				if err != nil {
					udpErrorsTotal.Inc()
					var ne net.Error
					if errors.As(err, &ne) {
						if ne.Temporary() {
							logger.Errorf("syslog: temporary error when listening for UDP at %q: %s", localAddr, err)
							time.Sleep(time.Second)
							continue
						}
						if strings.Contains(err.Error(), "use of closed network connection") {
							break
						}
					}
					logger.Errorf("syslog: cannot read UDP data from %s at %s: %s", remoteAddr, localAddr, err)
					continue
				}
				bb.B = bb.B[:n]
				udpRequestsTotal.Inc()
				if err := processStream(bb.NewReader(), cp); err != nil {
					logger.Errorf("syslog: cannot process UDP data from %s at %s: %s", remoteAddr, localAddr, err)
				}
			}
		}()
	}
	wg.Wait()
}

func serveTCP(ln net.Listener) {
	var cm ingestserver.ConnsMap
	cm.Init("syslog")

	var wg sync.WaitGroup
	addr := ln.Addr()
	for {
		c, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) {
				if ne.Temporary() {
					logger.Errorf("syslog: temporary error when listening for TCP addr %q: %s", addr, err)
					time.Sleep(time.Second)
					continue
				}
				if strings.Contains(err.Error(), "use of closed network connection") {
					break
				}
				logger.Fatalf("syslog: unrecoverable error when accepting TCP connections at %q: %s", addr, err)
			}
			logger.Fatalf("syslog: unexpected error when accepting TCP connections at %q: %s", addr, err)
		}
		if !cm.Add(c) {
			_ = c.Close()
			break
		}

		wg.Add(1)
		go func() {
			cp := insertutils.GetCommonParamsForSyslog(globalTenantID)
			if err := processStream(c, cp); err != nil {
				logger.Errorf("syslog: cannot process TCP data at %q: %s", addr, err)
			}

			cm.Delete(c)
			_ = c.Close()
			wg.Done()
		}()
	}

	cm.CloseAll(0)
	wg.Wait()
}

// processStream parses a stream of syslog messages from r and ingests them into vlstorage.
func processStream(r io.Reader, cp *insertutils.CommonParams) error {
	if err := vlstorage.CanWriteData(); err != nil {
		return err
	}

	lmp := cp.NewLogMessageProcessor()
	err := processStreamInternal(r, lmp)
	lmp.MustClose()

	return err
}

func processStreamInternal(r io.Reader, lmp insertutils.LogMessageProcessor) error {
	switch *compressMethod {
	case "", "none":
	case "gzip":
		zr, err := common.GetGzipReader(r)
		if err != nil {
			return fmt.Errorf("cannot read gzipped data: %w", err)
		}
		r = zr
	case "deflate":
		zr, err := common.GetZlibReader(r)
		if err != nil {
			return fmt.Errorf("cannot read deflated data: %w", err)
		}
		r = zr
	default:
		logger.Panicf("BUG: compressLevel=%q; supported values: none, gzip, deflate", *compressMethod)
	}

	err := processUncompressedStream(r, lmp)

	switch *compressMethod {
	case "gzip":
		zr := r.(*gzip.Reader)
		common.PutGzipReader(zr)
	case "deflate":
		zr := r.(io.ReadCloser)
		common.PutZlibReader(zr)
	}

	return err
}

func processUncompressedStream(r io.Reader, lmp insertutils.LogMessageProcessor) error {
	wcr := writeconcurrencylimiter.GetReader(r)
	defer writeconcurrencylimiter.PutReader(wcr)

	slr := getSyslogLineReader(wcr)
	defer putSyslogLineReader(slr)

	n := 0
	for {
		ok := slr.nextLine()
		wcr.DecConcurrency()
		if !ok {
			break
		}

		currentYear := int(globalCurrentYear.Load())
		err := processLine(slr.line, currentYear, globalTimezone, lmp)
		if err != nil {
			errorsTotal.Inc()
			return fmt.Errorf("cannot read line #%d: %s", n, err)
		}
		n++
		rowsIngestedTotal.Inc()
	}
	return slr.Error()
}

type syslogLineReader struct {
	line []byte

	br  *bufio.Reader
	err error
}

func (slr *syslogLineReader) reset(r io.Reader) {
	slr.line = slr.line[:0]
	slr.br.Reset(r)
	slr.err = nil
}

// Error returns the last error occurred in slr.
func (slr *syslogLineReader) Error() error {
	if slr.err == nil || slr.err == io.EOF {
		return nil
	}
	return slr.err
}

// nextLine reads the next syslog line from slr and stores it at slr.line.
//
// false is returned if the next line cannot be read. Error() must be called in this case
// in order to verify whether there is an error or just slr stream has been finished.
func (slr *syslogLineReader) nextLine() bool {
	if slr.err != nil {
		return false
	}

	prefix, err := slr.br.ReadSlice(' ')
	if err != nil {
		if err != io.EOF {
			slr.err = fmt.Errorf("cannot read message frame prefix: %w", err)
			return false
		}
		if len(prefix) == 0 {
			slr.err = err
			return false
		}
	}
	// skip empty lines
	for len(prefix) > 0 && prefix[0] == '\n' {
		prefix = prefix[1:]
	}

	if prefix[0] >= '0' && prefix[0] <= '9' {
		// This is octet-counting method. See https://www.ietf.org/archive/id/draft-gerhards-syslog-plain-tcp-07.html#msgxfer
		msgLenStr := bytesutil.ToUnsafeString(prefix[:len(prefix)-1])
		msgLen, err := strconv.ParseUint(msgLenStr, 10, 64)
		if err != nil {
			slr.err = fmt.Errorf("cannot parse message length from %q: %w", msgLenStr, err)
			return false
		}
		if maxMsgLen := insertutils.MaxLineSizeBytes.IntN(); msgLen > uint64(maxMsgLen) {
			slr.err = fmt.Errorf("cannot read message longer than %d bytes; msgLen=%d", maxMsgLen, msgLen)
			return false
		}
		slr.line = slicesutil.SetLength(slr.line, int(msgLen))
		if _, err := io.ReadFull(slr.br, slr.line); err != nil {
			slr.err = fmt.Errorf("cannot read message with size %d bytes: %w", msgLen, err)
			return false
		}
		return true
	}

	// This is octet-stuffing method. See https://www.ietf.org/archive/id/draft-gerhards-syslog-plain-tcp-07.html#octet-stuffing-legacy
	slr.line = append(slr.line[:0], prefix...)
	for {
		line, err := slr.br.ReadSlice('\n')
		if err == nil {
			slr.line = append(slr.line, line[:len(line)-1]...)
			return true
		}
		if err == io.EOF {
			slr.line = append(slr.line, line...)
			return true
		}
		if err == bufio.ErrBufferFull {
			slr.line = append(slr.line, line...)
			continue
		}
		slr.err = fmt.Errorf("cannot read message in octet-stuffing method: %w", err)
		return false
	}
}

func getSyslogLineReader(r io.Reader) *syslogLineReader {
	v := syslogLineReaderPool.Get()
	if v == nil {
		br := bufio.NewReaderSize(r, 64*1024)
		return &syslogLineReader{
			br: br,
		}
	}
	slr := v.(*syslogLineReader)
	slr.reset(r)
	return slr
}

func putSyslogLineReader(slr *syslogLineReader) {
	syslogLineReaderPool.Put(slr)
}

var syslogLineReaderPool sync.Pool

func processLine(line []byte, currentYear int, timezone *time.Location, lmp insertutils.LogMessageProcessor) error {
	p := logstorage.GetSyslogParser(currentYear, timezone)
	lineStr := bytesutil.ToUnsafeString(line)
	p.Parse(lineStr)
	ts, err := insertutils.ExtractTimestampISO8601FromFields("timestamp", p.Fields)
	if err != nil {
		return fmt.Errorf("cannot get timestamp from syslog line %q: %w", line, err)
	}
	logstorage.RenameField(p.Fields, "message", "_msg")
	lmp.AddRow(ts, p.Fields)
	logstorage.PutSyslogParser(p)

	return nil
}

var (
	rowsIngestedTotal = metrics.NewCounter(`vl_rows_ingested_total{type="syslog"}`)

	errorsTotal = metrics.NewCounter(`vl_errors_total{type="syslog"}`)

	udpRequestsTotal = metrics.NewCounter(`vl_udp_reqests_total{type="syslog"}`)
	udpErrorsTotal   = metrics.NewCounter(`vl_udp_errors_total{type="syslog"}`)
)