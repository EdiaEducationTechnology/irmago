package servercore

import (
	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/server"
	"github.com/sirupsen/logrus"
)

// This file contains the handler functions for the protocol messages, receiving and returning normally
// Go-typed messages here (JSON (un)marshalling is handled by the router).
// Maintaining the session state is done here, as well as checking whether the session is in the
// appropriate status before handling the request.

func (session *session) handleDelete() {
	if session.status.Finished() {
		return
	}
	session.markAlive()

	session.result = &server.SessionResult{Token: session.token, Status: server.StatusCancelled, Type: session.action}
	session.setStatus(server.StatusCancelled)
}

func (session *session) handleGetRequest(min, max *irma.ProtocolVersion) (irma.SessionRequest, *irma.RemoteError) {
	if session.status != server.StatusInitialized {
		return nil, server.RemoteError(server.ErrorUnexpectedRequest, "Session already started")
	}
	session.markAlive()

	var err error
	if session.version, err = chooseProtocolVersion(min, max); err != nil {
		return nil, session.fail(server.ErrorProtocolVersion, "")
	}
	session.conf.Logger.WithFields(logrus.Fields{"session": session.token, "version": session.version.String()}).Debugf("Protocol version negotiated")
	session.request.SetVersion(session.version)

	session.setStatus(server.StatusConnected)
	return session.request, nil
}

func (session *session) handleGetStatus() (server.Status, *irma.RemoteError) {
	return session.status, nil
}

func (session *session) handlePostSignature(signature *irma.SignedMessage) (*irma.ProofStatus, *irma.RemoteError) {
	if session.status != server.StatusConnected {
		return nil, server.RemoteError(server.ErrorUnexpectedRequest, "Session not yet started or already finished")
	}
	session.markAlive()

	var err error
	var rerr *irma.RemoteError
	session.result.Signature = signature
	session.result.Disclosed, session.result.ProofStatus, err = signature.Verify(
		session.conf.IrmaConfiguration, session.request.(*irma.SignatureRequest))
	if err == nil {
		session.setStatus(server.StatusDone)
	} else {
		if err == irma.ErrorMissingPublicKey {
			rerr = session.fail(server.ErrorUnknownPublicKey, err.Error())
		} else {
			rerr = session.fail(server.ErrorUnknown, err.Error())
		}
	}
	return &session.result.ProofStatus, rerr
}

func (session *session) handlePostDisclosure(disclosure irma.Disclosure) (*irma.ProofStatus, *irma.RemoteError) {
	if session.status != server.StatusConnected {
		return nil, server.RemoteError(server.ErrorUnexpectedRequest, "Session not yet started or already finished")
	}
	session.markAlive()

	var err error
	var rerr *irma.RemoteError
	session.result.Disclosed, session.result.ProofStatus, err = disclosure.Verify(
		session.conf.IrmaConfiguration, session.request.(*irma.DisclosureRequest))
	if err == nil {
		session.setStatus(server.StatusDone)
	} else {
		if err == irma.ErrorMissingPublicKey {
			rerr = session.fail(server.ErrorUnknownPublicKey, err.Error())
		} else {
			rerr = session.fail(server.ErrorUnknown, err.Error())
		}
	}
	return &session.result.ProofStatus, rerr
}

func (session *session) handlePostCommitments(commitments *irma.IssueCommitmentMessage) ([]*gabi.IssueSignatureMessage, *irma.RemoteError) {
	if session.status != server.StatusConnected {
		return nil, server.RemoteError(server.ErrorUnexpectedRequest, "Session not yet started or already finished")
	}
	session.markAlive()

	request := session.request.(*irma.IssuanceRequest)
	discloseCount := len(request.Disclose)
	if len(commitments.Proofs) != len(request.Credentials)+discloseCount {
		return nil, session.fail(server.ErrorAttributesMissing, "")
	}

	// Compute list of public keys against which to verify the received proofs
	disclosureproofs := irma.ProofList(commitments.Proofs[:discloseCount])
	pubkeys, err := disclosureproofs.ExtractPublicKeys(session.conf.IrmaConfiguration)
	if err != nil {
		return nil, session.fail(server.ErrorInvalidProofs, err.Error())
	}
	for _, cred := range request.Credentials {
		iss := cred.CredentialTypeID.IssuerIdentifier()
		pubkey, _ := session.conf.IrmaConfiguration.PublicKey(iss, cred.KeyCounter) // No error, already checked earlier
		pubkeys = append(pubkeys, pubkey)
	}

	// Verify and merge keyshare server proofs, if any
	for i, proof := range commitments.Proofs {
		pubkey := pubkeys[i]
		schemeid := irma.NewIssuerIdentifier(pubkey.Issuer).SchemeManagerIdentifier()
		if session.conf.IrmaConfiguration.SchemeManagers[schemeid].Distributed() {
			proofP, err := session.getProofP(commitments, schemeid)
			if err != nil {
				return nil, session.fail(server.ErrorKeyshareProofMissing, err.Error())
			}
			proof.MergeProofP(proofP, pubkey)
		}
	}

	// Verify all proofs and check disclosed attributes, if any, against request
	session.result.Disclosed, session.result.ProofStatus, err = commitments.Disclosure().VerifyAgainstDisjunctions(
		session.conf.IrmaConfiguration, request.Disclose, request.Context, request.Nonce, pubkeys, false)
	if err != nil {
		if err == irma.ErrorMissingPublicKey {
			return nil, session.fail(server.ErrorUnknownPublicKey, "")
		} else {
			return nil, session.fail(server.ErrorUnknown, "")
		}
	}
	if session.result.ProofStatus == irma.ProofStatusExpired {
		return nil, session.fail(server.ErrorAttributesExpired, "")
	}
	if session.result.ProofStatus != irma.ProofStatusValid {
		return nil, session.fail(server.ErrorInvalidProofs, "")
	}

	// Compute CL signatures
	var sigs []*gabi.IssueSignatureMessage
	for i, cred := range request.Credentials {
		id := cred.CredentialTypeID.IssuerIdentifier()
		pk, _ := session.conf.IrmaConfiguration.PublicKey(id, cred.KeyCounter)
		sk, _ := session.conf.PrivateKey(id)
		issuer := gabi.NewIssuer(sk, pk, one)
		proof := commitments.Proofs[i+discloseCount].(*gabi.ProofU)
		attributes, err := cred.AttributeList(session.conf.IrmaConfiguration, 0x03)
		if err != nil {
			return nil, session.fail(server.ErrorIssuanceFailed, err.Error())
		}
		sig, err := issuer.IssueSignature(proof.U, attributes.Ints, commitments.Nonce2)
		if err != nil {
			return nil, session.fail(server.ErrorIssuanceFailed, err.Error())
		}
		sigs = append(sigs, sig)
	}

	session.setStatus(server.StatusDone)
	return sigs, nil
}
