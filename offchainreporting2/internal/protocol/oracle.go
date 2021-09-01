package protocol

import (
	"context"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/smartcontractkit/libocr/internal/loghelper"
	"github.com/smartcontractkit/libocr/offchainreporting2/internal/config"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/subprocesses"
)

const futureMessageBufferSize = 10 // big enough for a couple of full rounds of repgen protocol

// RunOracle runs one oracle instance of the offchain reporting protocol and manages
// the lifecycle of all underlying goroutines.
//
// RunOracle runs forever until ctx is cancelled. It will only shut down
// after all its sub-goroutines have exited.
func RunOracle(
	ctx context.Context,

	config config.SharedConfig,
	contractTransmitter types.ContractTransmitter,
	database types.Database,
	id commontypes.OracleID,
	localConfig types.LocalConfig,
	logger loghelper.LoggerWithContext,
	netEndpoint NetworkEndpoint,
	offchainKeyring types.OffchainKeyring,
	onchainKeyring types.OnchainKeyring,
	reportingPlugin types.ReportingPlugin,
	reportQuorum int,
	telemetrySender TelemetrySender,
) {
	o := oracleState{
		ctx: ctx,

		Config:              config,
		contractTransmitter: contractTransmitter,
		database:            database,
		id:                  id,
		localConfig:         localConfig,
		logger:              logger,
		netEndpoint:         netEndpoint,
		OffchainKeyring:     offchainKeyring,
		onchainKeyring:      onchainKeyring,
		reportingPlugin:     reportingPlugin,
		reportQuorum:        reportQuorum,
		telemetrySender:     telemetrySender,
	}
	o.run()
}

type oracleState struct {
	ctx context.Context

	Config              config.SharedConfig
	contractTransmitter types.ContractTransmitter
	database            types.Database
	id                  commontypes.OracleID
	localConfig         types.LocalConfig
	logger              loghelper.LoggerWithContext
	netEndpoint         NetworkEndpoint
	OffchainKeyring     types.OffchainKeyring
	onchainKeyring      types.OnchainKeyring
	reportingPlugin     types.ReportingPlugin
	reportQuorum        int
	telemetrySender     TelemetrySender

	bufferedMessages          []*MessageBuffer
	chNetToPacemaker          chan<- MessageToPacemakerWithSender
	chNetToReportGeneration   chan<- MessageToReportGenerationWithSender
	chNetToReportFinalization chan<- MessageToReportFinalizationWithSender
	childCancel               context.CancelFunc
	childCtx                  context.Context
	epoch                     uint32
	subprocesses              subprocesses.Subprocesses
}

// run ensures safe shutdown of the Oracle's "child routines",
// (Pacemaker, ReportGeneration and Transmission) upon o.ctx.Done()
// being closed.
//
// TODO: update graph
// Here is a graph of the various channels involved and what they
// transport.
//
//      ┌─────────────epoch changes─────────────┐
//      ▼                                       │
//  ┌──────┐                               ┌────┴────┐
//  │Oracle├────pacemaker messages────────►│Pacemaker│
//  └────┬─┘                               └─────────┘
//       │                                       ▲
//       └──────rep. gen. messages────────────┐  │
//                                            ▼  │progress events
//  ┌────────────┐                         ┌─────┴──────────┐
//  │Transmission│◄──────reports───────────┤ReportGeneration│
//  └────────────┘                         └────────────────┘
//
// All channels are unbuffered.
//
// Once o.ctx.Done() is closed, the Oracle runloop will enter the
// corresponding select case and no longer forward network messages
// to Pacemaker and ReportGeneration. It will then cancel o.childCtx,
// making all children exit. To prevent deadlocks, all channel sends and
// receives in Oracle, Pacemaker, ReportGeneration, Transmission, etc...
// are contained in select{} statements that also contain a case for context
// cancellation.
//
// Finally, all sub-goroutines spawned in the protocol are attached to o.subprocesses
// (with the exception of ReportGeneration which is explicitly managed by Pacemaker).
// This enables us to wait for their completion before exiting.
func (o *oracleState) run() {
	o.logger.Info("Running", nil)

	for i := 0; i < o.Config.N(); i++ {
		o.bufferedMessages = append(o.bufferedMessages, NewMessageBuffer(futureMessageBufferSize))
	}

	chNetToPacemaker := make(chan MessageToPacemakerWithSender)
	o.chNetToPacemaker = chNetToPacemaker

	chNetToReportGeneration := make(chan MessageToReportGenerationWithSender)
	o.chNetToReportGeneration = chNetToReportGeneration

	chPacemakerToOracle := make(chan uint32)

	chNetToReportFinalization := make(chan MessageToReportFinalizationWithSender)
	o.chNetToReportFinalization = chNetToReportFinalization

	chReportGenerationToReportFinalization := make(chan EventToReportFinalization)

	chReportFinalizationToTransmission := make(chan EventToTransmission)

	o.childCtx, o.childCancel = context.WithCancel(context.Background())
	defer o.childCancel()

	o.subprocesses.Go(func() {
		RunPacemaker(
			o.childCtx,
			&o.subprocesses,

			chNetToPacemaker,
			chNetToReportGeneration,
			chPacemakerToOracle,
			chReportGenerationToReportFinalization,
			o.Config,
			o.contractTransmitter,
			o.database,
			o.id,
			o.localConfig,
			o.logger,
			o.netEndpoint,
			o.OffchainKeyring,
			o.onchainKeyring,
			o.reportingPlugin,
			o.reportQuorum,
			o.telemetrySender,
		)
	})
	o.subprocesses.Go(func() {
		RunReportFinalization(
			o.childCtx,
			&o.subprocesses,

			chNetToReportFinalization,
			chReportFinalizationToTransmission,
			chReportGenerationToReportFinalization,
			o.Config,
			o.onchainKeyring,
			o.logger,
			o.netEndpoint,
			o.reportQuorum,
		)
	})
	o.subprocesses.Go(func() {
		RunTransmission(
			o.childCtx,
			&o.subprocesses,

			o.Config,
			chReportFinalizationToTransmission,
			o.database,
			o.id,
			o.localConfig,
			o.logger,
			o.reportingPlugin,
			o.contractTransmitter,
		)
	})

	chNet := o.netEndpoint.Receive()

	chDone := o.ctx.Done()
	for {
		select {
		case msg := <-chNet:
			msg.Msg.process(o, msg.Sender)
		case epoch := <-chPacemakerToOracle:
			o.epochChanged(epoch)
		case <-chDone:
		}

		// ensure prompt exit
		select {
		case <-chDone:
			o.logger.Debug("Oracle: winding down", nil)
			o.childCancel()
			o.subprocesses.Wait()
			o.logger.Debug("Oracle: exiting", nil)
			return
		default:
		}
	}
}

func (o *oracleState) epochChanged(e uint32) {
	o.epoch = e
	o.logger.Trace("epochChanged: getting messages for new epoch", commontypes.LogFields{
		"epoch": e,
	})
	for id, buffer := range o.bufferedMessages {
		for {
			msg := buffer.Peek()
			if msg == nil {
				// no messages left in buffer
				break
			}
			msgEpoch := (*msg).epoch()
			if msgEpoch < e {
				// remove from buffer
				buffer.Pop()
				o.logger.Debug("epochChanged: unbuffered and dropped message", commontypes.LogFields{
					"remoteOracleID": id,
					"epoch":          e,
					"message":        *msg,
				})
			} else if msgEpoch == e {
				// remove from buffer
				buffer.Pop()

				o.logger.Trace("epochChanged: unbuffered messages for new epoch", commontypes.LogFields{
					"remoteOracleID": id,
					"epoch":          e,
					"message":        *msg,
				})
				o.chNetToReportGeneration <- MessageToReportGenerationWithSender{
					*msg,
					commontypes.OracleID(id),
				}
			} else { // msgEpoch > e
				// this and all subsequent messages are for future epochs
				// leave them in the buffer
				break
			}
		}
	}
	o.logger.Trace("epochChanged: done getting messages for new epoch", commontypes.LogFields{
		"epoch": e,
	})
}

func (o *oracleState) reportGenerationMessage(msg MessageToReportGeneration, sender commontypes.OracleID) {
	msgEpoch := msg.epoch()
	if msgEpoch < o.epoch {
		// drop
		o.logger.Debug("oracle: dropping message for past epoch", commontypes.LogFields{
			"epoch":  o.epoch,
			"sender": sender,
			"msg":    msg,
		})
	} else if msgEpoch == o.epoch {
		o.chNetToReportGeneration <- MessageToReportGenerationWithSender{msg, sender}
	} else {
		o.bufferedMessages[sender].Push(msg)
		o.logger.Trace("oracle: buffering message for future epoch", commontypes.LogFields{
			"epoch":  o.epoch,
			"sender": sender,
			"msg":    msg,
		})
	}
}
