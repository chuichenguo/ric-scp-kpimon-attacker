package control

import (
	"encoding/json"
	"errors"
	"gerrit.o-ran-sc.org/r/ric-plt/xapp-frame/pkg/xapp"
	"github.com/go-redis/redis"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"fmt"
)

type Control struct {
	ranList []string //nodeB list
	eventCreateExpired int32 //maximum time for the RIC Subscription Request event creation procedure in the E2 Node
	eventDeleteExpired int32 //maximum time for the RIC Subscription Request event deletion procedure in the E2 Node
	rcChan                chan *xapp.RMRParams //channel for receiving rmr message
	client                *redis.Client        //redis client
	eventCreateExpiredMap map[string]bool      //map for recording the RIC Subscription Request event creation procedure is expired or not
	eventDeleteExpiredMap map[string]bool      //map for recording the RIC Subscription Request event deletion procedure is expired or not
	eventCreateExpiredMu  *sync.Mutex          //mutex for eventCreateExpiredMap
	eventDeleteExpiredMu  *sync.Mutex          //mutex for eventDeleteExpiredMap
}

func init() {
	file := "/opt/kpimon.log"
	logFile, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0766)
	if err != nil {
		panic(err)
	}
	log.SetOutput(logFile)
	log.SetPrefix("[qSkipTool]")
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.LUTC)
	xapp.Logger.SetLevel(4)
}

func NewControl() Control {
	str := os.Getenv("ranList")
	return Control{strings.Split(str, ","),
		5, 5,
		make(chan *xapp.RMRParams),
		redis.NewClient(&redis.Options{
			Addr:     "10.244.0.14:6379",//os.Getenv("10.244.0.14:6379"), //"localhost:6379" "redisAddr"
			Password: "",
			DB:       0,
		}),
		make(map[string]bool),
		make(map[string]bool),
		&sync.Mutex{},
		&sync.Mutex{}}
}

func ReadyCB(i interface{}) {
	c := i.(*Control)

	c.startTimerSubReq()
	go c.controlLoop()
}

func (c *Control) Run() {
	_, err := c.client.Ping().Result()
	if err != nil {
		xapp.Logger.Error("Failed to connect to Redis DB with %v", err)
		log.Printf("Failed to connect to Redis DB with %v", err)
	}
	if len(c.ranList) > 0 {
		xapp.SetReadyCB(ReadyCB, c)
		xapp.Run(c)
	} else {
		xapp.Logger.Error("gNodeB not set for subscription")
		log.Printf("gNodeB not set for subscription")
	}

}

func (c *Control) startTimerSubReq() {
	timerSR := time.NewTimer(5 * time.Second)
	count := 0

	go func(t *time.Timer) {
		defer timerSR.Stop()
		for {
			<-t.C
			count++
			xapp.Logger.Debug("send RIC_SUB_REQ to gNodeB with cnt=%d", count)
			log.Printf("send RIC_SUB_REQ to gNodeB with cnt=%d", count)
			err := c.sendRicSubRequest(1001, 1001, 0)
			if err != nil && count < MAX_SUBSCRIPTION_ATTEMPTS {
				t.Reset(5 * time.Second)
			} else {
				break
			}
		}
	}(timerSR)
}

func (c *Control) Consume(rp *xapp.RMRParams) (err error) {
	c.rcChan <- rp
	return
}

func (c *Control) rmrSend(params *xapp.RMRParams) (err error) {
	if !xapp.Rmr.Send(params, false) {
		err = errors.New("rmr.Send() failed")
		xapp.Logger.Error("Failed to rmrSend to %v", err)
		log.Printf("Failed to rmrSend to %v", err)
	}
	return
}

func (c *Control) rmrReplyToSender(params *xapp.RMRParams) (err error) {
	if !xapp.Rmr.Send(params, true) {
		err = errors.New("rmr.Send() failed")
		xapp.Logger.Error("Failed to rmrReplyToSender to %v", err)
		log.Printf("Failed to rmrReplyToSender to %v", err)
	}
	return
}

func (c *Control) controlLoop() {
	for {
		msg := <-c.rcChan
		xapp.Logger.Debug("Received message type: %d", msg.Mtype)
		log.Printf("Received message type: %d", msg.Mtype)
		switch msg.Mtype {
		case 12050:
			c.handleIndication(msg) //注意!!!
		case 12011:
			c.handleSubscriptionResponse(msg)
		case 12012:
			c.handleSubscriptionFailure(msg)
		case 12021:
			c.handleSubscriptionDeleteResponse(msg)
		case 12022:
			c.handleSubscriptionDeleteFailure(msg)
		default:
			err := errors.New("Message Type " + strconv.Itoa(msg.Mtype) + " is discarded")
			xapp.Logger.Error("Unknown message type: %v", err)
			log.Printf("Unknown message type: %v", err)
		}
	}
}
/*---------------------------------------------START OF handleIndication---------------------------------------------*/
func (c *Control) handleIndication(params *xapp.RMRParams) (err error) {
	var e2ap *E2ap
	var e2sm *E2sm

	indicationMsg, err := e2ap.GetIndicationMessage(params.Payload)
	if err != nil { //skip
		xapp.Logger.Error("Failed to decode RIC Indication message: %v", err)
		log.Printf("Failed to decode RIC Indication message: %v", err)
		return
	}
	log.Printf("RIC Indication message from {%s} received", params.Meid.RanName)
	log.Printf("RequestID: %d", indicationMsg.RequestID)
	log.Printf("RequestSequenceNumber: %d", indicationMsg.RequestSequenceNumber)
	log.Printf("FunctionID: %d", indicationMsg.FuncID)
	log.Printf("ActionID: %d", indicationMsg.ActionID)
	log.Printf("IndicationSN: %d", indicationMsg.IndSN)
	log.Printf("IndicationType: %d", indicationMsg.IndType)
	log.Printf("IndicationHeader: %x", indicationMsg.IndHeader)
	log.Printf("IndicationMessage: %x", indicationMsg.IndMessage)
	log.Printf("CallProcessID: %x", indicationMsg.CallProcessID)

	indicationHdr, err := e2sm.GetIndicationHeader(indicationMsg.IndHeader)
	if err != nil {
		xapp.Logger.Error("Failed to decode RIC Indication Header: %v", err)
		log.Printf("Failed to decode RIC Indication Header: %v", err)
		return
	}

	var cellIDHdr string
	var plmnIDHdr string
	var sliceIDHdr int32
	var fiveQIHdr int64

	log.Printf("-----------RIC Indication Header-----------")
	if indicationHdr.IndHdrType == 1 { //indicationHdr.IndHdrType == 1
		log.Printf("RIC Indication Header Format: %d", indicationHdr.IndHdrType)
		indHdrFormat1 := indicationHdr.IndHdr.(*IndicationHeaderFormat1)

		log.Printf("GlobalKPMnodeIDType: %d", indHdrFormat1.GlobalKPMnodeIDType) //indHdrFormat1.GlobalKPMnodeIDType == 0
		if indHdrFormat1.GlobalKPMnodeIDType == 1 { //skip all
			globalKPMnodegNBID := indHdrFormat1.GlobalKPMnodeID.(GlobalKPMnodegNBIDType)

			globalgNBID := globalKPMnodegNBID.GlobalgNBID

			log.Printf("PlmnID: %x", globalgNBID.PlmnID.Buf)
			log.Printf("gNB ID Type: %d", globalgNBID.GnbIDType)
			if globalgNBID.GnbIDType == 1 {
				gNBID := globalgNBID.GnbID.(GNBID)
				log.Printf("gNB ID ID: %x, Unused: %d", gNBID.Buf, gNBID.BitsUnused)
			}

			if globalKPMnodegNBID.GnbCUUPID != nil {
				log.Printf("gNB-CU-UP ID: %x", globalKPMnodegNBID.GnbCUUPID.Buf)
			}

			if globalKPMnodegNBID.GnbDUID != nil {
				log.Printf("gNB-DU ID: %x", globalKPMnodegNBID.GnbDUID.Buf)
			}
		} else if indHdrFormat1.GlobalKPMnodeIDType == 2 {
			globalKPMnodeengNBID := indHdrFormat1.GlobalKPMnodeID.(GlobalKPMnodeengNBIDType)

			log.Printf("PlmnID: %x", globalKPMnodeengNBID.PlmnID.Buf)
			log.Printf("en-gNB ID Type: %d", globalKPMnodeengNBID.GnbIDType)
			if globalKPMnodeengNBID.GnbIDType == 1 {
				engNBID := globalKPMnodeengNBID.GnbID.(ENGNBID)
				log.Printf("en-gNB ID ID: %x, Unused: %d", engNBID.Buf, engNBID.BitsUnused)
			}
		} else if indHdrFormat1.GlobalKPMnodeIDType == 3 {
			globalKPMnodengeNBID := indHdrFormat1.GlobalKPMnodeID.(GlobalKPMnodengeNBIDType)

			log.Printf("PlmnID: %x", globalKPMnodengeNBID.PlmnID.Buf)
			log.Printf("ng-eNB ID Type: %d", globalKPMnodengeNBID.EnbIDType)
			if globalKPMnodengeNBID.EnbIDType == 1 {
				ngeNBID := globalKPMnodengeNBID.EnbID.(NGENBID_Macro)
				log.Printf("ng-eNB ID ID: %x, Unused: %d", ngeNBID.Buf, ngeNBID.BitsUnused)
			} else if globalKPMnodengeNBID.EnbIDType == 2 {
				ngeNBID := globalKPMnodengeNBID.EnbID.(NGENBID_ShortMacro)
				log.Printf("ng-eNB ID ID: %x, Unused: %d", ngeNBID.Buf, ngeNBID.BitsUnused)
			} else if globalKPMnodengeNBID.EnbIDType == 3 {
				ngeNBID := globalKPMnodengeNBID.EnbID.(NGENBID_LongMacro)
				log.Printf("ng-eNB ID ID: %x, Unused: %d", ngeNBID.Buf, ngeNBID.BitsUnused)
			}
		} else if indHdrFormat1.GlobalKPMnodeIDType == 4 {
			globalKPMnodeeNBID := indHdrFormat1.GlobalKPMnodeID.(GlobalKPMnodeeNBIDType)

			log.Printf("PlmnID: %x", globalKPMnodeeNBID.PlmnID.Buf)
			log.Printf("eNB ID Type: %d", globalKPMnodeeNBID.EnbIDType)
			if globalKPMnodeeNBID.EnbIDType == 1 {
				eNBID := globalKPMnodeeNBID.EnbID.(ENBID_Macro)
				log.Printf("eNB ID ID: %x, Unused: %d", eNBID.Buf, eNBID.BitsUnused)
			} else if globalKPMnodeeNBID.EnbIDType == 2 {
				eNBID := globalKPMnodeeNBID.EnbID.(ENBID_Home)
				log.Printf("eNB ID ID: %x, Unused: %d", eNBID.Buf, eNBID.BitsUnused)
			} else if globalKPMnodeeNBID.EnbIDType == 3 {
				eNBID := globalKPMnodeeNBID.EnbID.(ENBID_ShortMacro)
				log.Printf("eNB ID ID: %x, Unused: %d", eNBID.Buf, eNBID.BitsUnused)
			} else if globalKPMnodeeNBID.EnbIDType == 4 {
				eNBID := globalKPMnodeeNBID.EnbID.(ENBID_LongMacro)
				log.Printf("eNB ID ID: %x, Unused: %d", eNBID.Buf, eNBID.BitsUnused)
			}

		}
		//skip
		if indHdrFormat1.NRCGI != nil {

			log.Printf("nRCGI.PlmnID: %x", indHdrFormat1.NRCGI.PlmnID.Buf)
			log.Printf("nRCGI.NRCellID ID: %x, Unused: %d", indHdrFormat1.NRCGI.NRCellID.Buf, indHdrFormat1.NRCGI.NRCellID.BitsUnused)

			cellIDHdr, err = e2sm.ParseNRCGI(*indHdrFormat1.NRCGI)
			if err != nil {
				xapp.Logger.Error("Failed to parse NRCGI in RIC Indication Header: %v", err)
				log.Printf("Failed to parse NRCGI in RIC Indication Header: %v", err)
				return
			}
		} else {
			cellIDHdr = ""
		}
		//indHdrFormat1.PlmnID != nil
		if indHdrFormat1.PlmnID != nil {
			log.Printf("PlmnID: %x", indHdrFormat1.PlmnID.Buf) //PlmnID: 00f110
			/*fmt.Printf("-----------------EXPERIENCE-----------------\n")
			temp := []byte{0x0, 0xf1, 0x11}
			indHdrFormat1.PlmnID.Buf = temp
			fmt.Printf("%#v\n", indHdrFormat1)
			fmt.Printf("%#v\n", indHdrFormat1.PlmnID)
			fmt.Printf("%#v\n", indHdrFormat1.PlmnID.Buf)
			fmt.Printf("-----------------EXPERIENCE-----------------\n")*/
			plmnIDHdr, err = e2sm.ParsePLMNIdentity(indHdrFormat1.PlmnID.Buf, indHdrFormat1.PlmnID.Size)
			if err != nil {
				xapp.Logger.Error("Failed to parse PlmnID in RIC Indication Header: %v", err)
				log.Printf("Failed to parse PlmnID in RIC Indication Header: %v", err)
				return
			}
		} else {
			plmnIDHdr = ""
		}
		//skip
		if indHdrFormat1.SliceID != nil {
			log.Printf("SST: %x", indHdrFormat1.SliceID.SST.Buf)

			if indHdrFormat1.SliceID.SD != nil {
				log.Printf("SD: %x", indHdrFormat1.SliceID.SD.Buf)
			}

			sliceIDHdr, err = e2sm.ParseSliceID(*indHdrFormat1.SliceID)
			if err != nil {
				xapp.Logger.Error("Failed to parse SliceID in RIC Indication Header: %v", err)
				log.Printf("Failed to parse SliceID in RIC Indication Header: %v", err)
				return
			}
		} else {
			sliceIDHdr = -1
		}
		//skip
		if indHdrFormat1.FiveQI != -1 {
			log.Printf("5QI: %d", indHdrFormat1.FiveQI)
		}
		fiveQIHdr = indHdrFormat1.FiveQI

		if indHdrFormat1.Qci != -1 {
			log.Printf("QCI: %d", indHdrFormat1.Qci)
		}
	} else {
		xapp.Logger.Error("Unknown RIC Indication Header Format: %d", indicationHdr.IndHdrType)
		log.Printf("Unknown RIC Indication Header Format: %d", indicationHdr.IndHdrType)
		return
	}
	//skip
	indMsg, err := e2sm.GetIndicationMessage(indicationMsg.IndMessage)
	if err != nil {
		xapp.Logger.Error("Failed to decode RIC Indication Message: %v", err)
		log.Printf("Failed to decode RIC Indication Message: %v", err)
		return
	}

	var flag bool
	var containerType int32
	var timestampPDCPBytes *Timestamp
	var dlPDCPBytes int64
	var ulPDCPBytes int64
	var timestampPRB *Timestamp
	var availPRBDL int64
	var availPRBUL int64

	log.Printf("-----------RIC Indication Message-----------")
	log.Printf("StyleType: %d", indMsg.StyleType) //indMsg.StyleType == 4
	if indMsg.IndMsgType == 1 {//indMsg.IndMsgType == 1
		log.Printf("RIC Indication Message Format: %d", indMsg.IndMsgType)

		indMsgFormat1 := indMsg.IndMsg.(*IndicationMessageFormat1)

		log.Printf("PMContainerCount: %d", indMsgFormat1.PMContainerCount) //indMsgFormat1.PMContainerCount == 3 must be 3

		for i := 0; i < indMsgFormat1.PMContainerCount; i++ { //0,1,2
			flag = false
			timestampPDCPBytes = nil
			dlPDCPBytes = -1
			ulPDCPBytes = -1
			timestampPRB = nil
			availPRBDL = -1
			availPRBUL = -1

			log.Printf("PMContainer[%d]: ", i) //0,1,2

			pmContainer := indMsgFormat1.PMContainers[i]
			//fmt.Printf("%#v\n", pmContainer)
			if pmContainer.PFContainer != nil {
				containerType = pmContainer.PFContainer.ContainerType

				log.Printf("PFContainerType: %d", containerType) //pmContainer->containerType,0->1,1->2,2->3

				if containerType == 1 {
					log.Printf("oDU PF Container: ")

					oDU := pmContainer.PFContainer.Container.(*ODUPFContainerType)

					cellResourceReportCount := oDU.CellResourceReportCount
					log.Printf("CellResourceReportCount: %d", cellResourceReportCount) //cellResourceReportCount == 1

					for j := 0; j < cellResourceReportCount; j++ {
						log.Printf("CellResourceReport[%d]: ", j)

						cellResourceReport := oDU.CellResourceReports[j]

						log.Printf("nRCGI.PlmnID: %x", cellResourceReport.NRCGI.PlmnID.Buf) //nRCGI.PlmnID: 00f110
						log.Printf("nRCGI.nRCellID: %x", cellResourceReport.NRCGI.NRCellID.Buf) // nRCGI.nRCellID: 0000000010

						cellID, err := e2sm.ParseNRCGI(cellResourceReport.NRCGI)
						if err != nil { // skip
							xapp.Logger.Error("Failed to parse CellID in DU PF Container: %v", err)
							log.Printf("Failed to parse CellID in DU PF Container: %v", err)
							continue
						}
						if cellID == cellIDHdr {
							flag = true
						}

						log.Printf("TotalofAvailablePRBsDL: %d", cellResourceReport.TotalofAvailablePRBs.DL) //TotalofAvailablePRBsDL: -1
						log.Printf("TotalofAvailablePRBsUL: %d", cellResourceReport.TotalofAvailablePRBs.UL) //TotalofAvailablePRBsUL: -1

						if flag {
							availPRBDL = cellResourceReport.TotalofAvailablePRBs.DL
							availPRBUL = cellResourceReport.TotalofAvailablePRBs.UL
						}

						servedPlmnPerCellCount := cellResourceReport.ServedPlmnPerCellCount
						log.Printf("ServedPlmnPerCellCount: %d", servedPlmnPerCellCount) // ServedPlmnPerCellCount: 1

						for k := 0; k < servedPlmnPerCellCount; k++ {
							log.Printf("ServedPlmnPerCell[%d]: ", k) //ServedPlmnPerCell[0]: 

							servedPlmnPerCell := cellResourceReport.ServedPlmnPerCells[k]

							log.Printf("PlmnID: %x", servedPlmnPerCell.PlmnID.Buf) // PlmnID: 
							//skip
							if servedPlmnPerCell.DUPM5GC != nil {
								slicePerPlmnPerCellCount := servedPlmnPerCell.DUPM5GC.SlicePerPlmnPerCellCount
								log.Printf("SlicePerPlmnPerCellCount: %d", slicePerPlmnPerCellCount)

								for l := 0; l < slicePerPlmnPerCellCount; l++ {
									log.Printf("SlicePerPlmnPerCell[%d]: ", l)

									slicePerPlmnPerCell := servedPlmnPerCell.DUPM5GC.SlicePerPlmnPerCells[l]

									log.Printf("SliceID.sST: %x", slicePerPlmnPerCell.SliceID.SST.Buf)
									if slicePerPlmnPerCell.SliceID.SD != nil {
										log.Printf("SliceID.sD: %x", slicePerPlmnPerCell.SliceID.SD.Buf)
									}

									fQIPERSlicesPerPlmnPerCellCount := slicePerPlmnPerCell.FQIPERSlicesPerPlmnPerCellCount
									log.Printf("5QIPerSlicesPerPlmnPerCellCount: %d", fQIPERSlicesPerPlmnPerCellCount)

									for m := 0; m < fQIPERSlicesPerPlmnPerCellCount; m++ {
										log.Printf("5QIPerSlicesPerPlmnPerCell[%d]: ", m)

										fQIPERSlicesPerPlmnPerCell := slicePerPlmnPerCell.FQIPERSlicesPerPlmnPerCells[m]

										log.Printf("5QI: %d", fQIPERSlicesPerPlmnPerCell.FiveQI)
										log.Printf("PrbUsageDL: %d", fQIPERSlicesPerPlmnPerCell.PrbUsage.DL)
										log.Printf("PrbUsageUL: %d", fQIPERSlicesPerPlmnPerCell.PrbUsage.UL)
									}
								}
							}
							//skip
							if servedPlmnPerCell.DUPMEPC != nil {
								perQCIReportCount := servedPlmnPerCell.DUPMEPC.PerQCIReportCount
								log.Printf("PerQCIReportCount: %d", perQCIReportCount)

								for l := 0; l < perQCIReportCount; l++ {
									log.Printf("PerQCIReports[%d]: ", l)

									perQCIReport := servedPlmnPerCell.DUPMEPC.PerQCIReports[l]

									log.Printf("QCI: %d", perQCIReport.QCI)
									log.Printf("PrbUsageDL: %d", perQCIReport.PrbUsage.DL)
									log.Printf("PrbUsageUL: %d", perQCIReport.PrbUsage.UL)
								}
							}
						}
					}
				} else if containerType == 2 {
					log.Printf("oCU-CP PF Container: ") // oCU-CP PF Container: 

					oCUCP := pmContainer.PFContainer.Container.(*OCUCPPFContainerType)
					//skip
					if oCUCP.GNBCUCPName != nil {
						log.Printf("gNB-CU-CP Name: %x", oCUCP.GNBCUCPName.Buf)
					}

					log.Printf("NumberOfActiveUEs: %d", oCUCP.CUCPResourceStatus.NumberOfActiveUEs) // NumberOfActiveUEs: 0
				} else if containerType == 3 {
					log.Printf("oCU-UP PF Container: ") // oCU-UP PF Container: 

					oCUUP := pmContainer.PFContainer.Container.(*OCUUPPFContainerType)
					//skip
					if oCUUP.GNBCUUPName != nil {
						log.Printf("gNB-CU-UP Name: %x", oCUUP.GNBCUUPName.Buf)
					}

					cuUPPFContainerItemCount := oCUUP.CUUPPFContainerItemCount
					log.Printf("CU-UP PF Container Item Count: %d", cuUPPFContainerItemCount) // CU-UP PF Container Item Count: 1

					for j := 0; j < cuUPPFContainerItemCount; j++ {
						log.Printf("CU-UP PF Container Item [%d]: ", j) // CU-UP PF Container Item [0]: 

						cuUPPFContainerItem := oCUUP.CUUPPFContainerItems[j]

						log.Printf("InterfaceType: %d", cuUPPFContainerItem.InterfaceType) // InterfaceType: 2

						cuUPPlmnCount := cuUPPFContainerItem.OCUUPPMContainer.CUUPPlmnCount
						log.Printf("CU-UP Plmn Count: %d", cuUPPlmnCount) // CU-UP Plmn Count: 1

						for k := 0; k < cuUPPlmnCount; k++ {
							log.Printf("CU-UP Plmn [%d]: ",k) // CU-UP Plmn [0]: 

							cuUPPlmn := cuUPPFContainerItem.OCUUPPMContainer.CUUPPlmns[k]

							log.Printf("PlmnID: %x", cuUPPlmn.PlmnID.Buf) // PlmnID: 00f110

							plmnID, err := e2sm.ParsePLMNIdentity(cuUPPlmn.PlmnID.Buf, cuUPPlmn.PlmnID.Size)
							if err != nil { // skip
								xapp.Logger.Error("Failed to parse PlmnID in CU-UP PF Container: %v", err)
								log.Printf("Failed to parse PlmnID in CU-UP PF Container: %v", err)
								continue
							}
							//skip
							if cuUPPlmn.CUUPPM5GC != nil {
								sliceToReportCount := cuUPPlmn.CUUPPM5GC.SliceToReportCount
								log.Printf("SliceToReportCount: %d", sliceToReportCount)

								for l := 0; l < sliceToReportCount; l++ {
									log.Printf("SliceToReport[%d]: ", l)

									sliceToReport := cuUPPlmn.CUUPPM5GC.SliceToReports[l]

									log.Printf("SliceID.sST: %x", sliceToReport.SliceID.SST.Buf)
									if sliceToReport.SliceID.SD != nil {
										log.Printf("SliceID.sD: %x", sliceToReport.SliceID.SD.Buf)
									}

									sliceID, err := e2sm.ParseSliceID(sliceToReport.SliceID)
									if err != nil {
										xapp.Logger.Error("Failed to parse sliceID in CU-UP PF Container with PlmnID [%s]: %v", plmnID, err)
										log.Printf("Failed to parse sliceID in CU-UP PF Container with PlmnID [%s]: %v", plmnID, err)
										continue
									}

									fQIPERSlicesPerPlmnCount := sliceToReport.FQIPERSlicesPerPlmnCount
									log.Printf("5QIPerSlicesPerPlmnCount: %d", fQIPERSlicesPerPlmnCount)

									for m := 0; m < fQIPERSlicesPerPlmnCount; m++ {
										log.Printf("5QIPerSlicesPerPlmn[%d]: ", m)

										fQIPERSlicesPerPlmn := sliceToReport.FQIPERSlicesPerPlmns[m]

										fiveQI := fQIPERSlicesPerPlmn.FiveQI
										log.Printf("5QI: %d", fiveQI)

										if plmnID == plmnIDHdr && sliceID == sliceIDHdr && fiveQI == fiveQIHdr {
											flag = true
										}

										if fQIPERSlicesPerPlmn.PDCPBytesDL != nil {
											log.Printf("PDCPBytesDL: %x", fQIPERSlicesPerPlmn.PDCPBytesDL.Buf)

											if flag {
												dlPDCPBytes, err = e2sm.ParseInteger(fQIPERSlicesPerPlmn.PDCPBytesDL.Buf, fQIPERSlicesPerPlmn.PDCPBytesDL.Size)
												if err != nil {
													xapp.Logger.Error("Failed to parse PDCPBytesDL in CU-UP PF Container with PlmnID [%s], SliceID [%d], 5QI [%d]: %v", plmnID, sliceID, fiveQI, err)
													log.Printf("Failed to parse PDCPBytesDL in CU-UP PF Container with PlmnID [%s], SliceID [%d], 5QI [%d]: %v", plmnID, sliceID, fiveQI, err)
													continue
												}
											}
										}

										if fQIPERSlicesPerPlmn.PDCPBytesUL != nil {
											log.Printf("PDCPBytesUL: %x", fQIPERSlicesPerPlmn.PDCPBytesUL.Buf)

											if flag {
												ulPDCPBytes, err = e2sm.ParseInteger(fQIPERSlicesPerPlmn.PDCPBytesUL.Buf, fQIPERSlicesPerPlmn.PDCPBytesUL.Size)
												if err != nil {
													xapp.Logger.Error("Failed to parse PDCPBytesUL in CU-UP PF Container with PlmnID [%s], SliceID [%d], 5QI [%d]: %v", plmnID, sliceID, fiveQI, err)
													log.Printf("Failed to parse PDCPBytesUL in CU-UP PF Container with PlmnID [%s], SliceID [%d], 5QI [%d]: %v", plmnID, sliceID, fiveQI, err)
													continue
												}
											}
										}
									}
								}
							}
							//get into when NumberOfActiveUEs != 0, opening UL & DL
							if cuUPPlmn.CUUPPMEPC != nil {
								cuUPPMEPCPerQCIReportCount := cuUPPlmn.CUUPPMEPC.CUUPPMEPCPerQCIReportCount
								log.Printf("PerQCIReportCount: %d", cuUPPMEPCPerQCIReportCount) //PerQCIReportCount: 1

								for l := 0; l < cuUPPMEPCPerQCIReportCount; l++ {
									log.Printf("PerQCIReport[%d]: ",l) //PerQCIReport[0]: 

									cuUPPMEPCPerQCIReport := cuUPPlmn.CUUPPMEPC.CUUPPMEPCPerQCIReports[l]

									log.Printf("QCI: %d", cuUPPMEPCPerQCIReport.QCI) //QCI: 0

									if cuUPPMEPCPerQCIReport.PDCPBytesDL != nil {
										log.Printf("PDCPBytesDL: %x", cuUPPMEPCPerQCIReport.PDCPBytesDL.Buf) //PDCPBytesDL: 6750
									}
									if cuUPPMEPCPerQCIReport.PDCPBytesUL != nil {
										log.Printf("PDCPBytesUL: %x", cuUPPMEPCPerQCIReport.PDCPBytesUL.Buf) //PDCPBytesUL: 63c0
									}
								}
							}
						}
					}
				} else {
					xapp.Logger.Error("Unknown PF Container type: %d", containerType)
					log.Printf("Unknown PF Container type: %d", containerType)
					continue
				}
			}
			/*var attacker_output_file, err = os.OpenFile("attacker.txt", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0766)
			if err != nil {
				panic(err)
			}*/
			attacker_output_file, err := os.OpenFile("attacker.txt", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0766)
			if err != nil{
				fmt.Println("Open file err =", err)
			}
			fmt.Printf("--------------------EXP START--------------------\n")
			var ueMetrics *UeMetricsEntry
			ueMetrics = &UeMetricsEntry{}
			ueMetrics.MeasTimestampPRB.TVsec = 109
			ueMetrics.MeasTimestampPRB.TVnsec = 110
			var my_ueid int64 = 1001
			for k := 0; k < 10; k++ {
				ueMetrics.ServingCellID = string('A' + k)
				fmt.Printf("%#v\n", ueMetrics)
				
				newUeJsonStr, err := json.Marshal(ueMetrics)
				n, err := attacker_output_file.Write([]byte(newUeJsonStr))
				m, err_ := attacker_output_file.WriteString("\n")
				if err != nil {
					fmt.Println("Write file err =", err)
				}
				fmt.Println("Write file success, n =", n)
				if err_ != nil {
					fmt.Println("Write file err =", err)
				}
				fmt.Println("Write file success, m =", m)
				err = c.client.Set(strconv.FormatInt(my_ueid, 10), newUeJsonStr, 0).Err()
				if err != nil {
					fmt.Printf("--------------------EXP Insert Failed--------------------\n")
				}
				ueMetrics.MeasTimestampPRB.TVsec = ueMetrics.MeasTimestampPRB.TVsec + 1
				ueMetrics.MeasTimestampPRB.TVnsec = ueMetrics.MeasTimestampPRB.TVnsec + 1
				my_ueid = my_ueid + 1
				time.Sleep(2 * time.Second)
			}

			var my_ueid_ int64 = 1001
			for k := 0; k < 10; k++ {
				err = c.client.Del(strconv.FormatInt(my_ueid_, 10)).Err()
				if err != nil {
					fmt.Printf("--------------------EXP Delete Failed--------------------\n")
				}
				my_ueid_ = my_ueid_ + 1
				time.Sleep(2 * time.Second)
			}
			fmt.Printf("--------------------Sleep--------------------\n")
			time.Sleep(10 * time.Second)
			fmt.Printf("--------------------EXP END--------------------\n")
			//defer attacker_output_file.Close()
			//skip
			if pmContainer.RANContainer != nil {
				log.Printf("RANContainer: %x", pmContainer.RANContainer.Timestamp.Buf)

				timestamp, _ := e2sm.ParseTimestamp(pmContainer.RANContainer.Timestamp.Buf, pmContainer.RANContainer.Timestamp.Size)
				log.Printf("Timestamp=[sec: %d, nsec: %d]", timestamp.TVsec, timestamp.TVnsec)

				containerType = pmContainer.RANContainer.ContainerType
				if containerType == 1 {
					log.Printf("DU Usage Report: ")

					oDUUE := pmContainer.RANContainer.Container.(DUUsageReportType)

					for j := 0; j < oDUUE.CellResourceReportItemCount; j++ {
						cellResourceReportItem := oDUUE.CellResourceReportItems[j]

						log.Printf("nRCGI.PlmnID: %x", cellResourceReportItem.NRCGI.PlmnID.Buf)
						log.Printf("nRCGI.NRCellID: %x, Unused: %d", cellResourceReportItem.NRCGI.NRCellID.Buf, cellResourceReportItem.NRCGI.NRCellID.BitsUnused)

						servingCellID, err := e2sm.ParseNRCGI(cellResourceReportItem.NRCGI)
						if err != nil {
							xapp.Logger.Error("Failed to parse NRCGI in DU Usage Report: %v", err)
							log.Printf("Failed to parse NRCGI in DU Usage Report: %v", err)
							continue
						}

						for k := 0; k < cellResourceReportItem.UeResourceReportItemCount; k++ {
							ueResourceReportItem := cellResourceReportItem.UeResourceReportItems[k]

							log.Printf("C-RNTI: %x", ueResourceReportItem.CRNTI.Buf)

							ueID, err := e2sm.ParseInteger(ueResourceReportItem.CRNTI.Buf, ueResourceReportItem.CRNTI.Size)
							if err != nil {
								xapp.Logger.Error("Failed to parse C-RNTI in DU Usage Report with Serving Cell ID [%s]: %v", servingCellID, err)
								log.Printf("Failed to parse C-RNTI in DU Usage Report with Serving Cell ID [%s]: %v", servingCellID, err)
								continue
							}

							var ueMetrics *UeMetricsEntry
							if isUeExist, _ := c.client.Exists(strconv.FormatInt(ueID, 10)).Result(); isUeExist == 1 {
								ueJsonStr, _ := c.client.Get(strconv.FormatInt(ueID, 10)).Result()
								json.Unmarshal([]byte(ueJsonStr), ueMetrics)
							} else {
								ueMetrics = &UeMetricsEntry{}
							}

							ueMetrics.ServingCellID = servingCellID

							if flag {
								timestampPRB = timestamp
							}

							ueMetrics.MeasTimestampPRB.TVsec = timestamp.TVsec
							ueMetrics.MeasTimestampPRB.TVnsec = timestamp.TVnsec

							if ueResourceReportItem.PRBUsageDL != -1 {
								ueMetrics.PRBUsageDL = ueResourceReportItem.PRBUsageDL
							}

							if ueResourceReportItem.PRBUsageUL != -1 {
								ueMetrics.PRBUsageUL = ueResourceReportItem.PRBUsageUL
							}

							newUeJsonStr, err := json.Marshal(ueMetrics)
							if err != nil {
								xapp.Logger.Error("Failed to marshal UeMetrics with UE ID [%s]: %v", ueID, err)
								log.Printf("Failed to marshal UeMetrics with UE ID [%s]: %v", ueID, err)
								continue
							}
							err = c.client.Set(strconv.FormatInt(ueID, 10), newUeJsonStr, 0).Err()
							if err != nil {
								xapp.Logger.Error("Failed to set UeMetrics into redis with UE ID [%s]: %v", ueID, err)
								log.Printf("Failed to set UeMetrics into redis with UE ID [%s]: %v", ueID, err)
								continue
							}
						}
					}
				} else if containerType == 2 {
					log.Printf("CU-CP Usage Report: ")

					oCUCPUE := pmContainer.RANContainer.Container.(CUCPUsageReportType)

					for j := 0; j < oCUCPUE.CellResourceReportItemCount; j++ {
						cellResourceReportItem := oCUCPUE.CellResourceReportItems[j]

						log.Printf("nRCGI.PlmnID: %x", cellResourceReportItem.NRCGI.PlmnID.Buf)
						log.Printf("nRCGI.NRCellID: %x, Unused: %d", cellResourceReportItem.NRCGI.NRCellID.Buf, cellResourceReportItem.NRCGI.NRCellID.BitsUnused)

						servingCellID, err := e2sm.ParseNRCGI(cellResourceReportItem.NRCGI)
						if err != nil {
							xapp.Logger.Error("Failed to parse NRCGI in CU-CP Usage Report: %v", err)
							log.Printf("Failed to parse NRCGI in CU-CP Usage Report: %v", err)
							continue
						}

						for k := 0; k < cellResourceReportItem.UeResourceReportItemCount; k++ {
							ueResourceReportItem := cellResourceReportItem.UeResourceReportItems[k]

							log.Printf("C-RNTI: %x", ueResourceReportItem.CRNTI.Buf)

							ueID, err := e2sm.ParseInteger(ueResourceReportItem.CRNTI.Buf, ueResourceReportItem.CRNTI.Size)
							if err != nil {
								xapp.Logger.Error("Failed to parse C-RNTI in CU-CP Usage Report with Serving Cell ID [%s]: %v", err)
								log.Printf("Failed to parse C-RNTI in CU-CP Usage Report with Serving Cell ID [%s]: %v", err)
								continue
							}

							var ueMetrics *UeMetricsEntry
							if isUeExist, _ := c.client.Exists(strconv.FormatInt(ueID, 10)).Result(); isUeExist == 1 {
								ueJsonStr, _ := c.client.Get(strconv.FormatInt(ueID, 10)).Result()
								json.Unmarshal([]byte(ueJsonStr), ueMetrics)
							} else {
								ueMetrics = &UeMetricsEntry{}
							}

							ueMetrics.ServingCellID = servingCellID

							ueMetrics.MeasTimeRF.TVsec = timestamp.TVsec
							ueMetrics.MeasTimeRF.TVnsec = timestamp.TVnsec

							if ueResourceReportItem.ServingCellRF != nil {
								err = json.Unmarshal(ueResourceReportItem.ServingCellRF.Buf, &ueMetrics.ServingCellRF)
								if err != nil {
									xapp.Logger.Error("Failed to Unmarshal ServingCellRF in CU-CP Usage Report with UE ID [%s]: %v", ueID, err)
									log.Printf("Failed to Unmarshal ServingCellRF in CU-CP Usage Report with UE ID [%s]: %v", ueID, err)
									continue
								}
							}

							if ueResourceReportItem.NeighborCellRF != nil {
								err = json.Unmarshal(ueResourceReportItem.NeighborCellRF.Buf, &ueMetrics.NeighborCellsRF)
								if err != nil {
									xapp.Logger.Error("Failed to Unmarshal NeighborCellRF in CU-CP Usage Report with UE ID [%s]: %v", ueID, err)
									log.Printf("Failed to Unmarshal NeighborCellRF in CU-CP Usage Report with UE ID [%s]: %v", ueID, err)
									continue
								}
							}

							newUeJsonStr, err := json.Marshal(ueMetrics)
							if err != nil {
								xapp.Logger.Error("Failed to marshal UeMetrics with UE ID [%s]: %v", ueID, err)
								log.Printf("Failed to marshal UeMetrics with UE ID [%s]: %v", ueID, err)
								continue
							}
							err = c.client.Set(strconv.FormatInt(ueID, 10), newUeJsonStr, 0).Err()
							if err != nil {
								xapp.Logger.Error("Failed to set UeMetrics into redis with UE ID [%s]: %v", ueID, err)
								log.Printf("Failed to set UeMetrics into redis with UE ID [%s]: %v", ueID, err)
								continue
							}
						}
					}
				} else if containerType == 6 {
					log.Printf("CU-UP Usage Report: ")

					oCUUPUE := pmContainer.RANContainer.Container.(CUUPUsageReportType)

					for j := 0; j < oCUUPUE.CellResourceReportItemCount; j++ {
						cellResourceReportItem := oCUUPUE.CellResourceReportItems[j]

						log.Printf("nRCGI.PlmnID: %x", cellResourceReportItem.NRCGI.PlmnID.Buf)
						log.Printf("nRCGI.NRCellID: %x, Unused: %d", cellResourceReportItem.NRCGI.NRCellID.Buf, cellResourceReportItem.NRCGI.NRCellID.BitsUnused)

						servingCellID, err := e2sm.ParseNRCGI(cellResourceReportItem.NRCGI)
						if err != nil {
							xapp.Logger.Error("Failed to parse NRCGI in CU-UP Usage Report: %v", err)
							log.Printf("Failed to parse NRCGI in CU-UP Usage Report: %v", err)
							continue
						}

						for k := 0; k < cellResourceReportItem.UeResourceReportItemCount; k++ {
							ueResourceReportItem := cellResourceReportItem.UeResourceReportItems[k]

							log.Printf("C-RNTI: %x", ueResourceReportItem.CRNTI.Buf)

							ueID, err := e2sm.ParseInteger(ueResourceReportItem.CRNTI.Buf, ueResourceReportItem.CRNTI.Size)
							if err != nil {
								xapp.Logger.Error("Failed to parse C-RNTI in CU-UP Usage Report Serving Cell ID [%s]: %v", servingCellID, err)
								log.Printf("Failed to parse C-RNTI in CU-UP Usage Report Serving Cell ID [%s]: %v", servingCellID, err)
								continue
							}

							var ueMetrics *UeMetricsEntry
							if isUeExist, _ := c.client.Exists(strconv.FormatInt(ueID, 10)).Result(); isUeExist == 1 {
								ueJsonStr, _ := c.client.Get(strconv.FormatInt(ueID, 10)).Result()
								json.Unmarshal([]byte(ueJsonStr), ueMetrics)
							} else {
								ueMetrics = &UeMetricsEntry{}
							}

							ueMetrics.ServingCellID = servingCellID

							if flag {
								timestampPDCPBytes = timestamp
							}

							ueMetrics.MeasTimestampPDCPBytes.TVsec = timestamp.TVsec
							ueMetrics.MeasTimestampPDCPBytes.TVnsec = timestamp.TVnsec

							if ueResourceReportItem.PDCPBytesDL != nil {
								ueMetrics.PDCPBytesDL, err = e2sm.ParseInteger(ueResourceReportItem.PDCPBytesDL.Buf, ueResourceReportItem.PDCPBytesDL.Size)
								if err != nil {
									xapp.Logger.Error("Failed to parse PDCPBytesDL in CU-UP Usage Report with UE ID [%s]: %v", ueID, err)
									log.Printf("Failed to parse PDCPBytesDL in CU-UP Usage Report with UE ID [%s]: %v", ueID, err)
									continue
								}
							}

							if ueResourceReportItem.PDCPBytesUL != nil {
								ueMetrics.PDCPBytesUL, err = e2sm.ParseInteger(ueResourceReportItem.PDCPBytesUL.Buf, ueResourceReportItem.PDCPBytesUL.Size)
								if err != nil {
									xapp.Logger.Error("Failed to parse PDCPBytesUL in CU-UP Usage Report with UE ID [%s]: %v", ueID, err)
									log.Printf("Failed to parse PDCPBytesUL in CU-UP Usage Report with UE ID [%s]: %v", ueID, err)
									continue
								}
							}

							newUeJsonStr, err := json.Marshal(ueMetrics)
							if err != nil {
								xapp.Logger.Error("Failed to marshal UeMetrics with UE ID [%s]: %v", ueID, err)
								log.Printf("Failed to marshal UeMetrics with UE ID [%s]: %v", ueID, err)
								continue
							}
							err = c.client.Set(strconv.FormatInt(ueID, 10), newUeJsonStr, 0).Err()
							if err != nil {
								xapp.Logger.Error("Failed to set UeMetrics into redis with UE ID [%s]: %v", ueID, err)
								log.Printf("Failed to set UeMetrics into redis with UE ID [%s]: %v", ueID, err)
								continue
							}
						}
					}
				} else {
					xapp.Logger.Error("Unknown PF Container Type: %d", containerType)
					log.Printf("Unknown PF Container Type: %d", containerType)
					continue
				}
			}
			//skip
			if flag {
				var cellMetrics *CellMetricsEntry
				if isCellExist, _ := c.client.Exists(cellIDHdr).Result(); isCellExist == 1 {
					cellJsonStr, _ := c.client.Get(cellIDHdr).Result()
					json.Unmarshal([]byte(cellJsonStr), cellMetrics)
				} else {
					cellMetrics = &CellMetricsEntry{}
				}

				if timestampPDCPBytes != nil {
					cellMetrics.MeasTimestampPDCPBytes.TVsec = timestampPDCPBytes.TVsec
					cellMetrics.MeasTimestampPDCPBytes.TVnsec = timestampPDCPBytes.TVnsec
				}
				if dlPDCPBytes != -1 {
					cellMetrics.PDCPBytesDL = dlPDCPBytes
				}
				if ulPDCPBytes != -1 {
					cellMetrics.PDCPBytesUL = ulPDCPBytes
				}
				if timestampPRB != nil {
					cellMetrics.MeasTimestampPRB.TVsec = timestampPRB.TVsec
					cellMetrics.MeasTimestampPRB.TVnsec = timestampPRB.TVnsec
				}
				if availPRBDL != -1 {
					cellMetrics.AvailPRBDL = availPRBDL
				}
				if availPRBUL != -1 {
					cellMetrics.AvailPRBUL = availPRBUL
				}

				newCellJsonStr, err := json.Marshal(cellMetrics)
				if err != nil {
					xapp.Logger.Error("Failed to marshal CellMetrics with CellID [%s]: %v", cellIDHdr, err)
					log.Printf("Failed to marshal CellMetrics with CellID [%s]: %v", cellIDHdr, err)
					continue
				}
				err = c.client.Set(cellIDHdr, newCellJsonStr, 0).Err()
				if err != nil {
					xapp.Logger.Error("Failed to set CellMetrics into redis with CellID [%s]: %v", cellIDHdr, err)
					log.Printf("Failed to set CellMetrics into redis with CellID [%s]: %v", cellIDHdr, err)
					continue
				}
			}
		}
	} else {
		xapp.Logger.Error("Unknown RIC Indication Message Format: %d", indMsg.IndMsgType)
		log.Printf("Unkonw RIC Indication Message Format: %d", indMsg.IndMsgType)
		return
	}

	return nil
}
/*---------------------------------------------END OF handleIndication---------------------------------------------*/
func (c *Control) handleSubscriptionResponse(params *xapp.RMRParams) (err error) {
	xapp.Logger.Debug("The SubId in RIC_SUB_RESP is %d", params.SubId)
	log.Printf("The SubId in RIC_SUB_RESP is %d", params.SubId)

	ranName := params.Meid.RanName
	c.eventCreateExpiredMu.Lock()
	_, ok := c.eventCreateExpiredMap[ranName]
	if !ok {
		c.eventCreateExpiredMu.Unlock()
		xapp.Logger.Debug("RIC_SUB_REQ has been deleted!")
		log.Printf("RIC_SUB_REQ has been deleted!")
		return nil
	} else {
		c.eventCreateExpiredMap[ranName] = true
		c.eventCreateExpiredMu.Unlock()
	}

	var cep *E2ap
	subscriptionResp, err := cep.GetSubscriptionResponseMessage(params.Payload)
	if err != nil {
		xapp.Logger.Error("Failed to decode RIC Subscription Response message: %v", err)
		log.Printf("Failed to decode RIC Subscription Response message: %v", err)
		return
	}

	log.Printf("RIC Subscription Response message from {%s} received", params.Meid.RanName)
	log.Printf("SubscriptionID: %d", params.SubId)
	log.Printf("RequestID: %d", subscriptionResp.RequestID)
	log.Printf("RequestSequenceNumber: %d", subscriptionResp.RequestSequenceNumber)
	log.Printf("FunctionID: %d", subscriptionResp.FuncID)

	log.Printf("ActionAdmittedList:")
	for index := 0; index < subscriptionResp.ActionAdmittedList.Count; index++ {
		log.Printf("[%d]ActionID: %d", index, subscriptionResp.ActionAdmittedList.ActionID[index])
	}

	log.Printf("ActionNotAdmittedList:")
	for index := 0; index < subscriptionResp.ActionNotAdmittedList.Count; index++ {
		log.Printf("[%d]ActionID: %d", index, subscriptionResp.ActionNotAdmittedList.ActionID[index])
		log.Printf("[%d]CauseType: %d    CauseID: %d", index, subscriptionResp.ActionNotAdmittedList.Cause[index].CauseType, subscriptionResp.ActionNotAdmittedList.Cause[index].CauseID)
	}

	return nil
}

func (c *Control) handleSubscriptionFailure(params *xapp.RMRParams) (err error) {
	xapp.Logger.Debug("The SubId in RIC_SUB_FAILURE is %d", params.SubId)
	log.Printf("The SubId in RIC_SUB_FAILURE is %d", params.SubId)

	ranName := params.Meid.RanName
	c.eventCreateExpiredMu.Lock()
	_, ok := c.eventCreateExpiredMap[ranName]
	if !ok {
		c.eventCreateExpiredMu.Unlock()
		xapp.Logger.Debug("RIC_SUB_REQ has been deleted!")
		log.Printf("RIC_SUB_REQ has been deleted!")
		return nil
	} else {
		c.eventCreateExpiredMap[ranName] = true
		c.eventCreateExpiredMu.Unlock()
	}

	return nil
}

func (c *Control) handleSubscriptionDeleteResponse(params *xapp.RMRParams) (err error) {
	xapp.Logger.Debug("The SubId in RIC_SUB_DEL_RESP is %d", params.SubId)
	log.Printf("The SubId in RIC_SUB_DEL_RESP is %d", params.SubId)

	ranName := params.Meid.RanName
	c.eventDeleteExpiredMu.Lock()
	_, ok := c.eventDeleteExpiredMap[ranName]
	if !ok {
		c.eventDeleteExpiredMu.Unlock()
		xapp.Logger.Debug("RIC_SUB_DEL_REQ has been deleted!")
		log.Printf("RIC_SUB_DEL_REQ has been deleted!")
		return nil
	} else {
		c.eventDeleteExpiredMap[ranName] = true
		c.eventDeleteExpiredMu.Unlock()
	}

	return nil
}

func (c *Control) handleSubscriptionDeleteFailure(params *xapp.RMRParams) (err error) {
	xapp.Logger.Debug("The SubId in RIC_SUB_DEL_FAILURE is %d", params.SubId)
	log.Printf("The SubId in RIC_SUB_DEL_FAILURE is %d", params.SubId)

	ranName := params.Meid.RanName
	c.eventDeleteExpiredMu.Lock()
	_, ok := c.eventDeleteExpiredMap[ranName]
	if !ok {
		c.eventDeleteExpiredMu.Unlock()
		xapp.Logger.Debug("RIC_SUB_DEL_REQ has been deleted!")
		log.Printf("RIC_SUB_DEL_REQ has been deleted!")
		return nil
	} else {
		c.eventDeleteExpiredMap[ranName] = true
		c.eventDeleteExpiredMu.Unlock()
	}

	return nil
}

func (c *Control) setEventCreateExpiredTimer(ranName string) {
	c.eventCreateExpiredMu.Lock()
	c.eventCreateExpiredMap[ranName] = false
	c.eventCreateExpiredMu.Unlock()

	timer := time.NewTimer(time.Duration(c.eventCreateExpired) * time.Second)
	go func(t *time.Timer) {
		defer t.Stop()
		xapp.Logger.Debug("RIC_SUB_REQ[%s]: Waiting for RIC_SUB_RESP...", ranName)
		log.Printf("RIC_SUB_REQ[%s]: Waiting for RIC_SUB_RESP...", ranName)
		for {
			select {
			case <-t.C:
				c.eventCreateExpiredMu.Lock()
				isResponsed := c.eventCreateExpiredMap[ranName]
				delete(c.eventCreateExpiredMap, ranName)
				c.eventCreateExpiredMu.Unlock()
				if !isResponsed {
					xapp.Logger.Debug("RIC_SUB_REQ[%s]: RIC Event Create Timer experied!", ranName)
					log.Printf("RIC_SUB_REQ[%s]: RIC Event Create Timer experied!", ranName)
					// c.sendRicSubDelRequest(subID, requestSN, funcID)
					return
				}
			default:
				c.eventCreateExpiredMu.Lock()
				flag := c.eventCreateExpiredMap[ranName]
				if flag {
					delete(c.eventCreateExpiredMap, ranName)
					c.eventCreateExpiredMu.Unlock()
					xapp.Logger.Debug("RIC_SUB_REQ[%s]: RIC Event Create Timer canceled!", ranName)
					log.Printf("RIC_SUB_REQ[%s]: RIC Event Create Timer canceled!", ranName)
					return
				} else {
					c.eventCreateExpiredMu.Unlock()
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}(timer)
}

func (c *Control) setEventDeleteExpiredTimer(ranName string) {
	c.eventDeleteExpiredMu.Lock()
	c.eventDeleteExpiredMap[ranName] = false
	c.eventDeleteExpiredMu.Unlock()

	timer := time.NewTimer(time.Duration(c.eventDeleteExpired) * time.Second)
	go func(t *time.Timer) {
		defer t.Stop()
		xapp.Logger.Debug("RIC_SUB_DEL_REQ[%s]: Waiting for RIC_SUB_DEL_RESP...", ranName)
		log.Printf("RIC_SUB_DEL_REQ[%s]: Waiting for RIC_SUB_DEL_RESP...", ranName)
		for {
			select {
			case <-t.C:
				c.eventDeleteExpiredMu.Lock()
				isResponsed := c.eventDeleteExpiredMap[ranName]
				delete(c.eventDeleteExpiredMap, ranName)
				c.eventDeleteExpiredMu.Unlock()
				if !isResponsed {
					xapp.Logger.Debug("RIC_SUB_DEL_REQ[%s]: RIC Event Delete Timer experied!", ranName)
					log.Printf("RIC_SUB_DEL_REQ[%s]: RIC Event Delete Timer experied!", ranName)
					return
				}
			default:
				c.eventDeleteExpiredMu.Lock()
				flag := c.eventDeleteExpiredMap[ranName]
				if flag {
					delete(c.eventDeleteExpiredMap, ranName)
					c.eventDeleteExpiredMu.Unlock()
					xapp.Logger.Debug("RIC_SUB_DEL_REQ[%s]: RIC Event Delete Timer canceled!", ranName)
					log.Printf("RIC_SUB_DEL_REQ[%s]: RIC Event Delete Timer canceled!", ranName)
					return
				} else {
					c.eventDeleteExpiredMu.Unlock()
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}(timer)
}

func (c *Control) sendRicSubRequest(subID int, requestSN int, funcID int) (err error) {
	var e2ap *E2ap
	var e2sm *E2sm

	var eventTriggerCount int = 1
	var periods []int64 = []int64{13}
	var eventTriggerDefinition []byte = make([]byte, 8)
	_, err = e2sm.SetEventTriggerDefinition(eventTriggerDefinition, eventTriggerCount, periods)
	if err != nil {
		xapp.Logger.Error("Failed to send RIC_SUB_REQ: %v", err)
		log.Printf("Failed to send RIC_SUB_REQ: %v", err)
		return err
	}

	log.Printf("Set EventTriggerDefinition: %x", eventTriggerDefinition)

	var actionCount int = 1
	var ricStyleType []int64 = []int64{0}
	var actionIds []int64 = []int64{0}
	var actionTypes []int64 = []int64{0}
	var actionDefinitions []ActionDefinition = make([]ActionDefinition, actionCount)
	var subsequentActions []SubsequentAction = []SubsequentAction{SubsequentAction{0, 0, 0}}

	for index := 0; index < actionCount; index++ {
		if ricStyleType[index] == 0 {
			actionDefinitions[index].Buf = nil
			actionDefinitions[index].Size = 0
		} else {
			actionDefinitions[index].Buf = make([]byte, 8)
			_, err = e2sm.SetActionDefinition(actionDefinitions[index].Buf, ricStyleType[index])
			if err != nil {
				xapp.Logger.Error("Failed to send RIC_SUB_REQ: %v", err)
				log.Printf("Failed to send RIC_SUB_REQ: %v", err)
				return err
			}
			actionDefinitions[index].Size = len(actionDefinitions[index].Buf)

			log.Printf("Set ActionDefinition[%d]: %x", index, actionDefinitions[index].Buf)
		}
	}

	for index := 0; index < len(c.ranList); index++ {
		params := &xapp.RMRParams{}
		params.Mtype = 12010
		params.SubId = subID

		xapp.Logger.Debug("Send RIC_SUB_REQ to {%s}", c.ranList[index])
		log.Printf("Send RIC_SUB_REQ to {%s}", c.ranList[index])

		params.Payload = make([]byte, 1024)
		params.Payload, err = e2ap.SetSubscriptionRequestPayload(params.Payload, 1001, uint16(requestSN), uint16(funcID), eventTriggerDefinition, len(eventTriggerDefinition), actionCount, actionIds, actionTypes, actionDefinitions, subsequentActions)
		if err != nil {
			xapp.Logger.Error("Failed to send RIC_SUB_REQ: %v", err)
			log.Printf("Failed to send RIC_SUB_REQ: %v", err)
			return err
		}

		log.Printf("Set Payload: %x", params.Payload)

		params.Meid = &xapp.RMRMeid{RanName: c.ranList[index]}
		xapp.Logger.Debug("The RMR message to be sent is %d with SubId=%d", params.Mtype, params.SubId)
		log.Printf("The RMR message to be sent is %d with SubId=%d", params.Mtype, params.SubId)

		err = c.rmrSend(params)
		if err != nil {
			xapp.Logger.Error("Failed to send RIC_SUB_REQ: %v", err)
			log.Printf("Failed to send RIC_SUB_REQ: %v", err)
			return err
		}

		c.setEventCreateExpiredTimer(params.Meid.RanName)
		c.ranList = append(c.ranList[:index], c.ranList[index+1:]...)
		index--
	}

	return nil
}

func (c *Control) sendRicSubDelRequest(subID int, requestSN int, funcID int) (err error) {
	params := &xapp.RMRParams{}
	params.Mtype = 12020
	params.SubId = subID
	var e2ap *E2ap

	params.Payload = make([]byte, 1024)
	params.Payload, err = e2ap.SetSubscriptionDeleteRequestPayload(params.Payload, 100, uint16(requestSN), uint16(funcID))
	if err != nil {
		xapp.Logger.Error("Failed to send RIC_SUB_DEL_REQ: %v", err)
		return err
	}

	log.Printf("Set Payload: %x", params.Payload)

	if funcID == 0 {
		params.Meid = &xapp.RMRMeid{PlmnID: "::", EnbID: "::", RanName: "0"}
	} else {
		params.Meid = &xapp.RMRMeid{PlmnID: "::", EnbID: "::", RanName: "3"}
	}

	xapp.Logger.Debug("The RMR message to be sent is %d with SubId=%d", params.Mtype, params.SubId)
	log.Printf("The RMR message to be sent is %d with SubId=%d", params.Mtype, params.SubId)

	err = c.rmrSend(params)
	if err != nil {
		xapp.Logger.Error("Failed to send RIC_SUB_DEL_REQ: %v", err)
		log.Printf("Failed to send RIC_SUB_DEL_REQ: %v", err)
		return err
	}

	c.setEventDeleteExpiredTimer(params.Meid.RanName)

	return nil
}