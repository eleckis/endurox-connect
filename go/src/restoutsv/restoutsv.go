/*
** Enduro/X Outgoing http REST handler (HTTP client, XATMI server)
**
** @file restoutsv.go
** -----------------------------------------------------------------------------
** Enduro/X Middleware Platform for Distributed Transaction Processing
** Copyright (C) 2015, ATR Baltic, SIA. All Rights Reserved.
** This software is released under one of the following licenses:
** GPL or ATR Baltic's license for commercial use.
** -----------------------------------------------------------------------------
** GPL license:
**
** This program is free software; you can redistribute it and/or modify it under
** the terms of the GNU General Public License as published by the Free Software
** Foundation; either version 2 of the License, or (at your option) any later
** version.
**
** This program is distributed in the hope that it will be useful, but WITHOUT ANY
** WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
** PARTICULAR PURPOSE. See the GNU General Public License for more details.
**
** You should have received a copy of the GNU General Public License along with
** this program; if not, write to the Free Software Foundation, Inc., 59 Temple
** Place, Suite 330, Boston, MA 02111-1307 USA
**
** -----------------------------------------------------------------------------
** A commercial use license is available from ATR Baltic, SIA
** contact@atrbaltic.com
** -----------------------------------------------------------------------------
 */
package main

// Request types supported:
// - json (TypedJSON, TypedUBF)
// - plain text (TypedString)
// - binary (TypedCarray)

//Hmm we might need to put in channels a free ATMI contexts..
import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	u "ubftab"

	atmi "github.com/endurox-dev/endurox-go"
)

/*
#include <signal.h>
*/
import "C"

const (
	progsection = "@restout"
)

const (
	UNSET   = -1
	FALSE   = 0
	TRUE    = 1
	SUCCEED = atmi.SUCCEED
	FAIL    = atmi.FAIL
)

//Error handling type
const (
	ERRORS_HTTP = 1 //Return error code in http
	ERRORS_TEXT = 2 //Return error as formatted text (from config)
	ERRORS_JSON = 3 //Contact the json fields to main respons block.
	//Return the error code as UBF response (usable only in case if CONV_JSON2UBF used)
	ERRORS_JSON2UBF = 4
)

//Conversion types resolved
const (
	CONV_JSON2UBF = 1
	CONV_TEXT     = 2
	CONV_JSON     = 3
	CONV_RAW      = 4
)

//Defaults
const (
	ECHO_DEFAULT               = false
	ECHO_CONV_DEFAULT          = "json2ubf"
	ECHO_DATA_DEFAULT          = "{\"EX_DATA_STR\":\"Echo test\"}"
	ERRORS_DEFAULT             = ERRORS_JSON2UBF
	TIMEOUT_DEFAULT            = 60
	ERRFMT_JSON_MSG_DEFAULT    = "\"error_message\":\"%s\""
	ERRFMT_JSON_CODE_DEFAULT   = "\"error_code\":%d"
	ERRFMT_JSON_ONSUCC_DEFAULT = true /* generate success message in JSON */
	ERRFMT_TEXT_DEFAULT        = "%d: %s"
	WORKERS_DEFAULT            = 10 /* Number of worker processes */
	NOREQFILE_DEFAULT          = true
)

//We will have most of the settings as defaults
//And then these settings we can override with
type ServiceMap struct {
	Svc     string
	UrlBase string `json:"urlbase"`
	Url     string `json:"url"`

	Timeout int `json:"timeout"`

	Errors string `json:"errors"`
	//Above converted to consntant
	Errors_int int

	//Format for error to parse
	//for 'text'
	Errfmt_text string `json:"errfmt_text"`

	//JSON fields
	//for 'json'
	Errfmt_json_msg  string `json:"errfmt_json_msg"`
	Errfmt_json_code string `json:"errfmt_json_code"`
	//Should fields be present on success
	//If missing, then assume response is ok
	Errfmt_json_onsucc bool `json:"errfmt_json_onsucc"`

	//Error mapping between <http><Enduro/X, currently 0 or 11)
	Errors_fmt_http_map_str string `json:"errors_fmt_http_map"`
	Errors_fmt_http_map     map[string]*int

	//Should we parse the response (and fill the reply buffer)
	//in case if we got the error
	ParseOnError bool `json:"parseonerror"`

	//Do not sent request file request messages (for UBF2JSON)
	Noreqfilereq bool `json:"noreqfilereq"`

	//This is echo tester service
	Echo        bool   `json:"echo"`
	EchoTime    int    `json:"echo_time"`
	EchoMaxFail int    `json:"echo_max_fail"`
	EchoMinOK   int    `json:"echo_min_ok"`
	EchoConv    string `json:"echo_conv"`
	echoConvInt int
	EchoData    string `json:"echo_data"`

	//Counters:
	echoFails int //Number failed echos
	echoSucc  int //Number of ok echos

	DependsOn string `json:"depends_on"`

	//Wait for shutdown message
	shutdown chan bool //This is if we get shutdown messages

	//Preparsed buffers
	echoUBF    *atmi.TypedUBF
	echoCARRAY *atmi.TypedCarray

	//Dependies...
	Dependies []ServiceMap
}

var Mservices map[string]ServiceMap

//map the atmi error code (numbers + *) to some http error
//We shall provide default mappings.

var Mdefaults ServiceMap
var Mworkers int
var Mac *atmi.ATMICtx //Mainly shared for logging....

var Mmonitors int //Number of monitoring threads, to wait for shutdown.

var MmonitorsShut chan bool //Channel to wait for shutdown reply msgs

//Conversion types
var Mconvs = map[string]int{

	"json2ubf": CONV_JSON2UBF,
	"text":     CONV_TEXT,
	"json":     CONV_JSON,
	"raw":      CONV_RAW,
}

//Remap the error from string to int constant
//for better performance...
func remapErrors(svc *ServiceMap) error {

	switch svc.Errors {
	case "http":
		svc.Errors_int = ERRORS_HTTP
		break
	case "json":
		svc.Errors_int = ERRORS_JSON
		break
	case "json2ubf":
		svc.Errors_int = ERRORS_JSON2UBF
		break
	case "text":
		svc.Errors_int = ERRORS_TEXT
		break
	default:
		return fmt.Errorf("Unsupported error type [%s]", svc.Errors)
	}

	return nil
}

//Map the ATMI Errors to Http errors
//Format: <atmi_err>:<http_err>,<*>:<http_err>
//* - means any other unmapped ATMI error
//@param svc	Service map
func parseHTTPErrorMap(ac *atmi.ATMICtx, svc *ServiceMap) error {

	svc.Errors_fmt_http_map = make(map[string]*int)
	ac.TpLogDebug("Splitting error mapping string [%s]",
		svc.Errors_fmt_http_map_str)

	parsed := regexp.MustCompile(", *").Split(svc.Errors_fmt_http_map_str, -1)

	for index, element := range parsed {
		ac.TpLogDebug("Got pair [%s] at %d", element, index)

		pair := regexp.MustCompile(": *").Split(element, -1)

		pairLen := len(pair)

		if pairLen < 2 || pairLen > 2 {
			ac.TpLogError("Invalid http error pair: [%s] "+
				"parsed into %d elms", element, pairLen)

			return fmt.Errorf("Invalid http error pair: [%s] "+
				"parsed into %d elms", element, pairLen)
		}

		number, err := strconv.ParseInt(pair[1], 10, 0)

		if err != nil {
			ac.TpLogError("Failed to parse http error code %s (%s)",
				pair[1], err)
			return fmt.Errorf("Failed to parse http error code %s (%s)",
				pair[1], err)
		}

		//Add to hash
		n := int(number)
		svc.Errors_fmt_http_map[pair[0]] = &n
	}

	if nil == svc.Errors_fmt_http_map["*"] {
		return fmt.Errorf("Missing wildcard \"*\" in error config string!")
	}

	return nil
}

//Print the summary of the service after init
func printSvcSummary(ac *atmi.ATMICtx, svc *ServiceMap) {
	ac.TpLogWarn("Service: %s, Url: %s, [%s], Errors:%d (%s), Echo %t",
		svc.Svc,
		svc.Url,
		svc.Errors_int,
		svc.Errors,
		svc.Echo)
}

//Un-init function
func appinit(ctx *atmi.ATMICtx) int {
	//runtime.LockOSThread()

	Mservices = make(map[string]ServiceMap)

	//Setup default configuration
	Mdefaults.Errors_int = ERRORS_DEFAULT
	Mdefaults.Echo = ECHO_DEFAULT
	Mdefaults.EchoConv = ECHO_CONV_DEFAULT
	Mdefaults.EchoData = ECHO_DATA_DEFAULT
	Mdefaults.Errfmt_json_msg = ERRFMT_JSON_MSG_DEFAULT
	Mdefaults.Errfmt_json_code = ERRFMT_JSON_CODE_DEFAULT
	Mdefaults.Errfmt_json_onsucc = ERRFMT_JSON_ONSUCC_DEFAULT
	Mdefaults.Errfmt_text = ERRFMT_TEXT_DEFAULT
	Mdefaults.Noreqfilereq = NOREQFILE_DEFAULT

	Mworkers = WORKERS_DEFAULT

	//Get the configuration

	buf, err := ctx.NewUBF(16 * 1024)
	if nil != err {
		ctx.TpLog(atmi.LOG_ERROR, "Failed to allocate buffer: [%s]", err.Error())
		return FAIL
	}

	buf.BChg(u.EX_CC_CMD, 0, "g")
	buf.BChg(u.EX_CC_LOOKUPSECTION, 0, fmt.Sprintf("%s/%s", progsection,
		os.Getenv("NDRX_CCTAG")))

	if _, err := ctx.TpCall("@CCONF", buf, 0); nil != err {
		ctx.TpLog(atmi.LOG_ERROR, "ATMI Error %d:[%s]\n", err.Code(),
			err.Message())
		return FAIL
	}

	buf.TpLogPrintUBF(atmi.LOG_DEBUG, "Got configuration.")

	//Set the parameters (ip/port/services)

	occs, _ := buf.BOccur(u.EX_CC_KEY)
	// Load in the config...
	for occ := 0; occ < occs; occ++ {
		ctx.TpLog(atmi.LOG_DEBUG, "occ %d", occ)
		fldName, err := buf.BGetString(u.EX_CC_KEY, occ)

		if nil != err {
			ctx.TpLog(atmi.LOG_ERROR, "Failed to get field "+
				"%d occ %d", u.EX_CC_KEY, occ)
			return FAIL
		}

		ctx.TpLog(atmi.LOG_DEBUG, "Got config field [%s]", fldName)

		switch fldName {

		case "workers":
			Mworkers, _ = buf.BGetInt(u.EX_CC_VALUE, occ)
			break
		case "gencore":
			gencore, _ := buf.BGetInt(u.EX_CC_VALUE, occ)

			if TRUE == gencore {
				//Process signals by default handlers
				ctx.TpLogInfo("gencore=1 - SIGSEG signal will be " +
					"processed by default OS handler")
				// Have some core dumps...
				C.signal(11, nil)
			}
			break
		case "defaults":
			//Override the defaults
			jsonDefault, _ := buf.BGetByteArr(u.EX_CC_VALUE, occ)

			jerr := json.Unmarshal(jsonDefault, &Mdefaults)
			if jerr != nil {
				ctx.TpLog(atmi.LOG_ERROR,
					"Failed to parse defaults: %s", jerr.Error())
				return FAIL
			}

			if Mdefaults.Errors_fmt_http_map_str != "" {
				if jerr := parseHTTPErrorMap(ctx, &Mdefaults); jerr != nil {
					return FAIL
				}
			}

			remapErrors(&Mdefaults)

			if Mdefaults.Echo {
				Mdefaults.echoConvInt = Mconvs[Mdefaults.EchoConv]
				if Mdefaults.echoConvInt == 0 {
					ctx.TpLogError("Invalid conv: %s",
						Mdefaults.EchoConv)
					return FAIL
				}
			}

			printSvcSummary(ctx, &Mdefaults)

			break
		default:
			//Assign the defaults

			//Load services...

			match, _ := regexp.MatchString("^service\\s*.*$", fldName)

			if match {

				re := regexp.MustCompile("^service\\s*(.*)$")
				matchSvc := re.FindStringSubmatch(fldName)

				cfgVal, _ := buf.BGetString(u.EX_CC_VALUE, occ)

				ctx.TpLogInfo("Got service route config [%s]=[%s]",
					matchSvc[0], cfgVal)

				tmp := Mdefaults

				//Override the stuff from current config
				tmp.Svc = matchSvc[0]

				//err := json.Unmarshal(cfgVal, &tmp)
				decoder := json.NewDecoder(strings.NewReader(cfgVal))
				//conf := Config{}
				err := decoder.Decode(&tmp)

				if err != nil {
					ctx.TpLog(atmi.LOG_ERROR,
						"Failed to parse config key %s: %s",
						fldName, err)
					return FAIL
				}

				ctx.TpLogDebug("Got route: URL [%s] -> Service [%s]",
					fldName, tmp.Svc)
				tmp.Url = fldName

				//Parse http errors for
				if tmp.Errors_fmt_http_map_str != "" {
					if jerr := parseHTTPErrorMap(ctx, &tmp); jerr != nil {
						return FAIL
					}
				}

				remapErrors(&tmp)
				printSvcSummary(ctx, &tmp)

				//Add to HTTP listener
				//We should add service to advertise list...
				//And list if echo is enabled & succeeed
				//or if echo not set, then auto advertise all
				//http.HandleFunc(fldName, dispatchRequest)

				if strings.HasPrefix(tmp.Url, "/") {
					//This is partial URL, so use base
					tmp.Url = tmp.UrlBase + tmp.Url
				}

				if tmp.Echo {
					tmp.echoConvInt = Mconvs[tmp.EchoConv]
					if tmp.echoConvInt == 0 {
						ctx.TpLogError("Invalid conv: %s",
							tmp.EchoConv)
						return FAIL
					}

					if errA := tmp.PreparseEchoBuffers(ac); nil != arrA {
						ctx.TpLogError("Failed to parse "+
							"echo buffers: %s",
							errA.Error())
						return FAIL
					}

					//Make async chan
					tmp.shutdown = make(chan bool, 2)
					Mmonitors++
				}

				Mservices[matchSvc[0]] = tmp
			}
			break
		}
	}

	ctx.TpLogInfo("Number of monitor services: %s", Mmonitors)
	MmonitorsShut = make(chan bool, Mmonitors)

	//Add the default erorr mappings
	if Mdefaults.Errors_fmt_http_map_str == "" {

		//https://golang.org/src/net/http/status.go
		Mdefaults.Errors_fmt_http_map = make(map[string]*int)
		//Accepted
		tpeminval := atmi.TPMINVAL
		Mdefaults.Errors_fmt_http_map[strconv.Itoa(http.StatusOK)] = &tpeminval

		tpetime := atmi.TPETIME
		Mdefaults.Errors_fmt_http_map[strconv.Itoa(http.StatusGatewayTimeout)] = &tpetime

		tpnoent := atmi.TPENOENT
		Mdefaults.Errors_fmt_http_map[strconv.Itoa(http.StatusNotFound)] = &tpnoent

		//Anything other goes to server error.
		genfail := atmi.TPESVCFAIL
		Mdefaults.Errors_fmt_http_map["*"] = &genfail

	}

	ctx.TpLogInfo("About to init woker pool, number of workers: %d", Mworkers)

	if err := initPool(ctx, &MoutXPool); nil != err {
		return FAIL
	}

	//Advertise services which are not dependent
	for _, v := range Mservices {
		if v.DependsOn == "" || v.Echo {
			//Advertize service
			if errA := v.Advertise(ctx); nil != errA {
				return FAIL
			}
		} else if v.DependsOn != "" {
			//TODO: add current service to targets Dependies...
		}
	}

	return SUCCEED
}

//RESTOUT service - generic entry point
//@param ac ATMI Context
//@param svc Service call information
func RESTOUT(ac *atmi.ATMICtx, svc *atmi.TPSVCINFO) {

	ret := SUCCEED

	//Return to the caller
	defer func() {

		ac.TpLogCloseReqFile()
		if SUCCEED == ret {
			/* ac.TpContinue() - No need for this
			 * Or it have nothing todo.
			 * as operation  must be last.
			 */
			ac.TpContinue()
		} else {
			ac.TpReturn(atmi.TPFAIL, 0, &svc.Data, 0)
		}
	}()

	//Get UBF Handler
	ub, _ := ac.CastToUBF(&svc.Data)

	//Print the buffer to stdout
	ub.TpLogPrintUBF(atmi.LOG_DEBUG, "Incoming request:")

	//Resize buffer, to have some more space
	buf_size, err := ub.BUsed()

	if err != nil {
		ac.TpLogError("Failed to get incoming buffer used space: %d:%s",
			err.Code(), err.Message())
		ret = FAIL
		return
	}

	//Realloc to have some free space for buffer manipulations
	if err := ub.TpRealloc(buf_size + 1024); err != nil {
		ac.TpLogError("TpRealloc() Got error: %d:[%s]", err.Code(), err.Message())
		ret = FAIL
		return
	}

	//Pack the request data to pass to thread
	ctxData, err := ac.TpSrvGetCtxData()
	if nil != err {
		ac.TpLogError("Failed to get context data - dropping request",
			err.Code(), err.Message())
		ret = FAIL
		return
	}

	ac.TpLogInfo("Waiting for free XATMI out object")
	nr := getFreeXChan(ac, &MoutXPool)
	ac.TpLogInfo("Got XATMI out object")

	go XATMIDispatchCall(&MoutXPool, nr, ctxData, &svc.Data, svc.Cd, svc.Name)

	//runtime.GC()

	return
}

//Un-init & Terminate the application
func unInit(ac *atmi.ATMICtx) {

	//dispatch to monitors & wait for them to complete the shutdown
	for _, v := range Mservices {

		//Send shutdown to svc
		if v.Echo {
			ac.TpLogInfo("Shutting down monitor: [%s]", v.Svc)
			v.shutdown <- true
		}
	}

	for i := 0; i < Mmonitors; i++ {

		ac.TpLogInfo("Waiting monitor %d to complete", i)
		_ = <-MmonitorsShut
	}

	ac.TpLogInfo("Shutdown ok")

}

//Executable main entry point
func main() {
	//Have some context
	ac, err := atmi.NewATMICtx()

	if nil != err {
		fmt.Fprintf(os.Stderr, "Failed to allocate new context: %s", err)
		os.Exit(atmi.FAIL)
	} else {
		//Run as server
		if err = ac.TpRun(appinit, unInit); nil != err {
			ac.TpLogError("Exit with failure")
			os.Exit(atmi.FAIL)
		} else {
			ac.TpLogInfo("Exit with success")
			os.Exit(atmi.SUCCEED)
		}
	}
}