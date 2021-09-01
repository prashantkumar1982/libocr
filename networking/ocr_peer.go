package networking

import (
	"github.com/smartcontractkit/libocr/commontypes"
	ocr1types "github.com/smartcontractkit/libocr/offchainreporting/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"
)

type ocrBinaryNetworkEndpointFactory struct {
	*concretePeer
}

var _ ocr1types.BinaryNetworkEndpointFactory = (*ocrBinaryNetworkEndpointFactory)(nil)

const (
	// MaxOCRMsgLength is the maximum allowed length for a data payload in bytes
	// This is exported as serialization tests depend on it.
	// NOTE: This is slightly larger than 2x of the largest message we can
	// possibly send, assuming N=31.
	MaxOCRMsgLength = 10000
)

func (o *ocrBinaryNetworkEndpointFactory) NewEndpoint(
	configDigest ocr1types.ConfigDigest,
	pids []string,
	v1bootstrappers []string,
	v2bootstrappers []commontypes.BootstrapperLocator,
	failureThreshold int,
	messagesRatePerOracle float64,
	messagesCapacityPerOracle int,
) (commontypes.BinaryNetworkEndpoint, error) {
	var expandedConfigDigest ocr2types.ConfigDigest
	copy(expandedConfigDigest[:], configDigest[:])

	return o.concretePeer.newEndpoint(
		o.concretePeer.networkingStack,
		expandedConfigDigest,
		pids,
		v1bootstrappers,
		v2bootstrappers,
		failureThreshold,
		BinaryNetworkEndpointLimits{
			MaxOCRMsgLength,
			messagesRatePerOracle,
			messagesCapacityPerOracle,
			messagesRatePerOracle * MaxOCRMsgLength,
			messagesCapacityPerOracle * MaxOCRMsgLength,
		},
	)
}

type ocrBootstrapperFactory struct {
	*concretePeer
}

func (o *ocrBootstrapperFactory) NewBootstrapper(
	configDigest ocr1types.ConfigDigest,
	peerIDs []string,
	v1bootstrappers []string,
	v2bootstrappers []commontypes.BootstrapperLocator,
	failureThreshold int,
) (commontypes.Bootstrapper, error) {
	var expandedConfigDigest ocr2types.ConfigDigest
	copy(expandedConfigDigest[:], configDigest[:])

	return o.concretePeer.newBootstrapper(
		o.concretePeer.networkingStack,
		expandedConfigDigest,
		peerIDs,
		v1bootstrappers,
		v2bootstrappers,
		failureThreshold,
	)
}
