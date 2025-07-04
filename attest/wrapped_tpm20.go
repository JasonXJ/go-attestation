// Copyright 2020 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package attest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/google/go-tpm/legacy/tpm2"
	"github.com/google/go-tpm/tpmutil"
)

// wrappedTPM20 interfaces with a TPM 2.0 command channel.
type wrappedTPM20 struct {
	interf           TPMInterface
	rwc              CommandChannelTPM20
	tpmRSAEkTemplate *tpm2.Public
	tpmECCEkTemplate *tpm2.Public
}

// certifyingKey contains details of a TPM key that could certify other keys.
type certifyingKey struct {
	handle tpmutil.Handle
	alg    Algorithm
}

func (t *wrappedTPM20) rsaEkTemplate() tpm2.Public {
	if t.tpmRSAEkTemplate != nil {
		return *t.tpmRSAEkTemplate
	}

	nonce, err := tpm2.NVReadEx(t.rwc, nvramRSAEkNonceIndex, tpm2.HandleOwner, "", 0)
	if err != nil {
		t.tpmRSAEkTemplate = &defaultRSAEKTemplate // No nonce, use the default template
	} else {
		template := defaultRSAEKTemplate
		copy(template.RSAParameters.ModulusRaw, nonce)
		t.tpmRSAEkTemplate = &template
	}

	return *t.tpmRSAEkTemplate
}

func (t *wrappedTPM20) eccEkTemplate() tpm2.Public {
	if t.tpmECCEkTemplate != nil {
		return *t.tpmECCEkTemplate
	}

	nonce, err := tpm2.NVReadEx(t.rwc, nvramECCEkNonceIndex, tpm2.HandleOwner, "", 0)
	if err != nil {
		t.tpmECCEkTemplate = &defaultECCEKTemplate // No nonce, use the default template
	} else {
		template := defaultECCEKTemplate
		copy(template.ECCParameters.Point.XRaw, nonce)
		t.tpmECCEkTemplate = &template
	}

	return *t.tpmECCEkTemplate
}

func (t *wrappedTPM20) close() error {
	return t.rwc.Close()
}

// Info returns information about the TPM.
func (t *wrappedTPM20) info() (*TPMInfo, error) {
	var (
		tInfo = TPMInfo{
			Interface: t.interf,
		}
		t2Info tpmInfo
		err    error
	)

	if t2Info, err = readVendorAttributes(t.rwc); err != nil {
		return nil, err
	}
	tInfo.Manufacturer = t2Info.manufacturer
	tInfo.VendorInfo = t2Info.vendor
	tInfo.FirmwareVersionMajor = t2Info.fwMajor
	tInfo.FirmwareVersionMinor = t2Info.fwMinor
	return &tInfo, nil
}

// Return value: handle, whether we generated a new one, error.
func (t *wrappedTPM20) getEndorsementKeyHandle(ek *EK) (tpmutil.Handle, bool, error) {
	var ekHandle tpmutil.Handle
	var ekTemplate tpm2.Public

	if ek == nil {
		// The default is RSA for backward compatibility.
		ekHandle = commonRSAEkEquivalentHandle
		ekTemplate = t.rsaEkTemplate()
	} else {
		ekHandle = ek.handle
		if ekHandle == 0 {
			// Assume RSA EK handle if it was not provided.
			ekHandle = commonRSAEkEquivalentHandle
		}
		switch pub := ek.Public.(type) {
		case *rsa.PublicKey:
			ekTemplate = t.rsaEkTemplate()
		case *ecdsa.PublicKey:
			ekTemplate = t.eccEkTemplate()
		default:
			return 0, false, fmt.Errorf("unsupported public key type %T", pub)
		}
	}

	_, _, _, err := tpm2.ReadPublic(t.rwc, ekHandle)
	if err == nil {
		// Found the persistent handle, assume it's the key we want.
		return ekHandle, false, nil
	}
	rerr := err // Preserve this failure for later logging, if needed

	keyHnd, _, err := tpm2.CreatePrimary(t.rwc, tpm2.HandleEndorsement, tpm2.PCRSelection{}, "", "", ekTemplate)
	if err != nil {
		return 0, false, fmt.Errorf("ReadPublic failed (%v), and then CreatePrimary failed: %v", rerr, err)
	}
	defer tpm2.FlushContext(t.rwc, keyHnd)

	err = tpm2.EvictControl(t.rwc, "", tpm2.HandleOwner, keyHnd, ekHandle)
	if err != nil {
		return 0, false, fmt.Errorf("EvictControl failed: %v", err)
	}

	return ekHandle, true, nil
}

// Return value: handle, whether we generated a new one, error
func (t *wrappedTPM20) getStorageRootKeyHandle(parent ParentKeyConfig) (tpmutil.Handle, bool, error) {
	srkHandle := parent.Handle
	_, _, _, err := tpm2.ReadPublic(t.rwc, srkHandle)
	if err == nil {
		// Found the persistent handle, assume it's the key we want.
		return srkHandle, false, nil
	}
	rerr := err // Preserve this failure for later logging, if needed

	var srkTemplate tpm2.Public
	switch parent.Algorithm {
	case RSA:
		srkTemplate = defaultRSASRKTemplate
	case ECDSA:
		srkTemplate = defaultECCSRKTemplate
	default:
		return 0, false, fmt.Errorf("unsupported SRK algorithm: %v", parent.Algorithm)
	}
	keyHnd, _, err := tpm2.CreatePrimary(t.rwc, tpm2.HandleOwner, tpm2.PCRSelection{}, "", "", srkTemplate)
	if err != nil {
		return 0, false, fmt.Errorf("ReadPublic failed (%v), and then CreatePrimary failed: %v", rerr, err)
	}
	defer tpm2.FlushContext(t.rwc, keyHnd)

	err = tpm2.EvictControl(t.rwc, "", tpm2.HandleOwner, keyHnd, srkHandle)
	if err != nil {
		return 0, false, fmt.Errorf("EvictControl failed: %v", err)
	}

	return srkHandle, true, nil
}

func (t *wrappedTPM20) ekCertificates() ([]EK, error) {
	var res []EK
	if rsaCert, err := readEKCertFromNVRAM20(t.rwc, nvramRSACertIndex); err == nil {
		res = append(res, EK{Public: crypto.PublicKey(rsaCert.PublicKey), Certificate: rsaCert, handle: commonRSAEkEquivalentHandle})
	}
	if eccCert, err := readEKCertFromNVRAM20(t.rwc, nvramECCCertIndex); err == nil {
		res = append(res, EK{Public: crypto.PublicKey(eccCert.PublicKey), Certificate: eccCert, handle: commonECCEkEquivalentHandle})
	}
	return res, nil
}

func (t *wrappedTPM20) eks() ([]EK, error) {
	if cert, err := readEKCertFromNVRAM20(t.rwc, nvramRSACertIndex); err == nil {
		return []EK{
			{Public: crypto.PublicKey(cert.PublicKey), Certificate: cert, handle: commonRSAEkEquivalentHandle},
		}, nil
	}

	// Attempt to create an EK.
	ekHnd, _, err := tpm2.CreatePrimary(t.rwc, tpm2.HandleEndorsement, tpm2.PCRSelection{}, "", "", t.rsaEkTemplate())
	if err != nil {
		return nil, fmt.Errorf("EK CreatePrimary failed: %v", err)
	}
	defer tpm2.FlushContext(t.rwc, ekHnd)

	pub, _, _, err := tpm2.ReadPublic(t.rwc, ekHnd)
	if err != nil {
		return nil, fmt.Errorf("EK ReadPublic failed: %v", err)
	}
	if pub.RSAParameters == nil {
		return nil, errors.New("ECC EK not yet supported")
	}

	i, err := t.info()
	if err != nil {
		return nil, fmt.Errorf("retrieving TPM info failed: %v", err)
	}
	ekPub := &rsa.PublicKey{
		E: int(pub.RSAParameters.Exponent()),
		N: pub.RSAParameters.Modulus(),
	}
	certificateURL := ekCertURL(ekPub, i.Manufacturer.String())
	return []EK{
		{
			Public:         ekPub,
			CertificateURL: certificateURL,
			handle:         commonRSAEkEquivalentHandle,
		},
	}, nil
}

func (t *wrappedTPM20) newAK(opts *AKConfig) (*AK, error) {
	var parent ParentKeyConfig
	if opts != nil && opts.Parent != nil {
		parent = *opts.Parent
	} else {
		parent = defaultParentConfig
	}
	srk, _, err := t.getStorageRootKeyHandle(parent)
	if err != nil {
		return nil, fmt.Errorf("failed to get SRK handle: %v", err)
	}

	var akTemplate tpm2.Public
	var sigScheme *tpm2.SigScheme
	// The default is RSA.
	if opts != nil && opts.Algorithm == ECDSA {
		akTemplate = akTemplateECC
		sigScheme = akTemplateECC.ECCParameters.Sign
	} else {
		akTemplate = akTemplateRSA
		sigScheme = akTemplateRSA.RSAParameters.Sign
	}
	blob, pub, creationData, creationHash, tix, err := tpm2.CreateKey(t.rwc, srk, tpm2.PCRSelection{}, "", "", akTemplate)
	if err != nil {
		return nil, fmt.Errorf("CreateKeyEx() failed: %v", err)
	}
	keyHandle, _, err := tpm2.Load(t.rwc, srk, "", pub, blob)
	if err != nil {
		return nil, fmt.Errorf("Load() failed: %v", err)
	}
	// If any errors occur, free the AK's handle.
	defer func() {
		if err != nil {
			tpm2.FlushContext(t.rwc, keyHandle)
		}
	}()

	// We can only certify the creation immediately afterwards, so we cache the result.
	attestation, sig, err := tpm2.CertifyCreation(t.rwc, "", keyHandle, keyHandle, nil, creationHash, *sigScheme, tix)
	if err != nil {
		return nil, fmt.Errorf("CertifyCreation failed: %v", err)
	}
	return &AK{ak: newWrappedAK20(keyHandle, blob, pub, creationData, attestation, sig)}, nil
}

func (t *wrappedTPM20) newKey(ak *AK, opts *KeyConfig) (*Key, error) {
	k, ok := ak.ak.(*wrappedKey20)
	if !ok {
		return nil, fmt.Errorf("expected *wrappedKey20, got: %T", k)
	}

	kAlg, err := k.algorithm()
	if err != nil {
		return nil, fmt.Errorf("get algorithm: %v", err)
	}
	ck := certifyingKey{handle: k.hnd, alg: kAlg}
	return t.newKeyCertifiedByKey(ck, opts)
}

func (t *wrappedTPM20) newKeyCertifiedByKey(ck certifyingKey, opts *KeyConfig) (*Key, error) {
	parent, blob, pub, creationData, err := createKey(t, opts)
	if err != nil {
		return nil, fmt.Errorf("cannot create key: %v", err)
	}

	keyHandle, _, err := tpm2.Load(t.rwc, parent, "", pub, blob)
	if err != nil {
		return nil, fmt.Errorf("Load() failed: %v", err)
	}
	// If any errors occur, free the handle.
	defer func() {
		if err != nil {
			tpm2.FlushContext(t.rwc, keyHandle)
		}
	}()

	// Certify application key by AK
	certifyOpts := CertifyOpts{QualifyingData: opts.QualifyingData}
	cp, err := certifyByKey(t, keyHandle, ck, certifyOpts)
	if err != nil {
		return nil, fmt.Errorf("certifyByKey() failed: %v", err)
	}
	if !bytes.Equal(pub, cp.Public) {
		return nil, fmt.Errorf("certified incorrect key, expected: %v, certified: %v", pub, cp.Public)
	}

	// Pack the raw structure into a TPMU_SIGNATURE.
	tpmPub, err := tpm2.DecodePublic(pub)
	if err != nil {
		return nil, fmt.Errorf("decode public key: %v", err)
	}
	pubKey, err := tpmPub.Key()
	if err != nil {
		return nil, fmt.Errorf("access public key: %v", err)
	}
	return &Key{key: newWrappedKey20(keyHandle, blob, pub, creationData, cp.CreateAttestation, cp.CreateSignature), pub: pubKey, tpm: t}, nil
}

func createKey(t *wrappedTPM20, opts *KeyConfig) (tpmutil.Handle, []byte, []byte, []byte, error) {
	var parent ParentKeyConfig
	if opts != nil && opts.Parent != nil {
		parent = *opts.Parent
	} else {
		parent = defaultParentConfig
	}
	srk, _, err := t.getStorageRootKeyHandle(parent)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("failed to get SRK handle: %v", err)
	}

	tmpl, err := templateFromConfig(opts)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("incorrect key options: %v", err)
	}

	blob, pub, creationData, _, _, err := tpm2.CreateKey(t.rwc, srk, tpm2.PCRSelection{}, "", "", tmpl)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("CreateKey() failed: %v", err)
	}

	return srk, blob, pub, creationData, err
}

func templateFromConfig(opts *KeyConfig) (tpm2.Public, error) {
	var tmpl tpm2.Public
	switch opts.Algorithm {
	case RSA:
		tmpl = rsaKeyTemplate
		if opts.Size < 0 || opts.Size > 65535 { // basic sanity check
			return tmpl, fmt.Errorf("incorrect size parameter")
		}
		tmpl.RSAParameters.KeyBits = uint16(opts.Size)

	case ECDSA:
		tmpl = ecdsaKeyTemplate
		switch opts.Size {
		case 256:
			tmpl.NameAlg = tpm2.AlgSHA256
			tmpl.ECCParameters.Sign.Hash = tpm2.AlgSHA256
			tmpl.ECCParameters.CurveID = tpm2.CurveNISTP256
			tmpl.ECCParameters.Point = tpm2.ECPoint{
				XRaw: make([]byte, 32),
				YRaw: make([]byte, 32),
			}
		case 384:
			tmpl.NameAlg = tpm2.AlgSHA384
			tmpl.ECCParameters.Sign.Hash = tpm2.AlgSHA384
			tmpl.ECCParameters.CurveID = tpm2.CurveNISTP384
			tmpl.ECCParameters.Point = tpm2.ECPoint{
				XRaw: make([]byte, 48),
				YRaw: make([]byte, 48),
			}
		case 521:
			tmpl.NameAlg = tpm2.AlgSHA512
			tmpl.ECCParameters.Sign.Hash = tpm2.AlgSHA512
			tmpl.ECCParameters.CurveID = tpm2.CurveNISTP521
			tmpl.ECCParameters.Point = tpm2.ECPoint{
				XRaw: make([]byte, 65),
				YRaw: make([]byte, 65),
			}
		default:
			return tmpl, fmt.Errorf("unsupported key size: %v", opts.Size)
		}
	default:
		return tmpl, fmt.Errorf("unsupported algorithm type: %q", opts.Algorithm)
	}

	return tmpl, nil
}

func (t *wrappedTPM20) deserializeAndLoad(opaqueBlob []byte, parent ParentKeyConfig) (tpmutil.Handle, *serializedKey, error) {
	sKey, err := deserializeKey(opaqueBlob)
	if err != nil {
		return 0, nil, fmt.Errorf("deserializeKey() failed: %v", err)
	}
	if sKey.Encoding != keyEncodingEncrypted {
		return 0, nil, fmt.Errorf("unsupported key encoding: %x", sKey.Encoding)
	}

	srk, _, err := t.getStorageRootKeyHandle(parent)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get SRK handle: %v", err)
	}
	var hnd tpmutil.Handle
	if hnd, _, err = tpm2.Load(t.rwc, srk, "", sKey.Public, sKey.Blob); err != nil {
		return 0, nil, fmt.Errorf("Load() failed: %v", err)
	}
	return hnd, sKey, nil
}

func (t *wrappedTPM20) loadAK(opaqueBlob []byte) (*AK, error) {
	return t.loadAKWithParent(opaqueBlob, defaultParentConfig)
}

func (t *wrappedTPM20) loadAKWithParent(opaqueBlob []byte, parent ParentKeyConfig) (*AK, error) {
	hnd, sKey, err := t.deserializeAndLoad(opaqueBlob, parent)
	if err != nil {
		return nil, fmt.Errorf("cannot load attestation key: %v", err)
	}
	return &AK{ak: newWrappedAK20(hnd, sKey.Blob, sKey.Public, sKey.CreateData, sKey.CreateAttestation, sKey.CreateSignature)}, nil
}

func (t *wrappedTPM20) loadKey(opaqueBlob []byte) (*Key, error) {
	return t.loadKeyWithParent(opaqueBlob, defaultParentConfig)
}

func (t *wrappedTPM20) loadKeyWithParent(opaqueBlob []byte, parent ParentKeyConfig) (*Key, error) {
	hnd, sKey, err := t.deserializeAndLoad(opaqueBlob, parent)
	if err != nil {
		return nil, fmt.Errorf("cannot load signing key: %v", err)
	}
	tpmPub, err := tpm2.DecodePublic(sKey.Public)
	if err != nil {
		return nil, fmt.Errorf("decode public blob: %v", err)
	}
	pub, err := tpmPub.Key()
	if err != nil {
		return nil, fmt.Errorf("access public key: %v", err)
	}
	return &Key{key: newWrappedKey20(hnd, sKey.Blob, sKey.Public, sKey.CreateData, sKey.CreateAttestation, sKey.CreateSignature), pub: pub, tpm: t}, nil
}

func (t *wrappedTPM20) pcrbanks() ([]HashAlg, error) {
	return pcrbanks(t.rwc)
}

func (t *wrappedTPM20) pcrs(alg HashAlg) ([]PCR, error) {
	PCRs, err := readAllPCRs(t.rwc, alg.goTPMAlg())
	if err != nil {
		return nil, fmt.Errorf("failed to read PCRs: %v", err)
	}

	out := make([]PCR, len(PCRs))
	for index, digest := range PCRs {
		digestAlg, err := alg.cryptoHash()
		if err != nil {
			return nil, fmt.Errorf("unknown algorithm ID %x: %v", alg, err)
		}
		out[int(index)] = PCR{
			Index:     int(index),
			Digest:    digest,
			DigestAlg: digestAlg,
		}
	}

	return out, nil
}

func (t *wrappedTPM20) measurementLog() ([]byte, error) {
	return t.rwc.MeasurementLog()
}

// wrappedKey20 represents a key manipulated through a *wrappedTPM20.
type wrappedKey20 struct {
	hnd tpmutil.Handle

	blob              []byte
	public            []byte // used by both TPM1.2 and 2.0
	createData        []byte
	createAttestation []byte
	createSignature   []byte
}

func newWrappedAK20(hnd tpmutil.Handle, blob, public, createData, createAttestation, createSig []byte) ak {
	return &wrappedKey20{
		hnd:               hnd,
		blob:              blob,
		public:            public,
		createData:        createData,
		createAttestation: createAttestation,
		createSignature:   createSig,
	}
}

func newWrappedKey20(hnd tpmutil.Handle, blob, public, createData, createAttestation, createSig []byte) key {
	return &wrappedKey20{
		hnd:               hnd,
		blob:              blob,
		public:            public,
		createData:        createData,
		createAttestation: createAttestation,
		createSignature:   createSig,
	}
}

func (k *wrappedKey20) marshal() ([]byte, error) {
	return (&serializedKey{
		Encoding:   keyEncodingEncrypted,
		TPMVersion: 2,

		Blob:              k.blob,
		Public:            k.public,
		CreateData:        k.createData,
		CreateAttestation: k.createAttestation,
		CreateSignature:   k.createSignature,
	}).Serialize()
}

func (k *wrappedKey20) close(t tpmBase) error {
	tpm, ok := t.(*wrappedTPM20)
	if !ok {
		return fmt.Errorf("expected *wrappedTPM20, got %T", t)
	}
	return tpm2.FlushContext(tpm.rwc, k.hnd)
}

func (k *wrappedKey20) activateCredential(tb tpmBase, in EncryptedCredential, ek *EK) ([]byte, error) {
	t, ok := tb.(*wrappedTPM20)
	if !ok {
		return nil, fmt.Errorf("expected *wrappedTPM20, got %T", tb)
	}

	if len(in.Credential) < 2 {
		return nil, fmt.Errorf("malformed credential blob")
	}
	credential := in.Credential[2:]
	if len(in.Secret) < 2 {
		return nil, fmt.Errorf("malformed encrypted secret")
	}
	secret := in.Secret[2:]

	ekHnd, _, err := t.getEndorsementKeyHandle(ek)
	if err != nil {
		return nil, err
	}

	sessHandle, _, err := tpm2.StartAuthSession(
		t.rwc,
		tpm2.HandleNull,  /*tpmKey*/
		tpm2.HandleNull,  /*bindKey*/
		make([]byte, 16), /*nonceCaller*/
		nil,              /*secret*/
		tpm2.SessionPolicy,
		tpm2.AlgNull,
		tpm2.AlgSHA256)
	if err != nil {
		return nil, fmt.Errorf("creating session: %v", err)
	}
	defer tpm2.FlushContext(t.rwc, sessHandle)

	if _, _, err := tpm2.PolicySecret(t.rwc, tpm2.HandleEndorsement, tpm2.AuthCommand{Session: tpm2.HandlePasswordSession, Attributes: tpm2.AttrContinueSession}, sessHandle, nil, nil, nil, 0); err != nil {
		return nil, fmt.Errorf("tpm2.PolicySecret() failed: %v", err)
	}

	return tpm2.ActivateCredentialUsingAuth(t.rwc, []tpm2.AuthCommand{
		{Session: tpm2.HandlePasswordSession, Attributes: tpm2.AttrContinueSession},
		{Session: sessHandle, Attributes: tpm2.AttrContinueSession},
	}, k.hnd, ekHnd, credential, secret)
}

func sigSchemeFromAlgorithm(alg Algorithm) (tpm2.SigScheme, error) {
	switch alg {
	case RSA:
		return tpm2.SigScheme{
			Alg:  tpm2.AlgRSASSA,
			Hash: tpm2.AlgSHA256,
		}, nil
	case ECDSA:
		return tpm2.SigScheme{
			Alg:  tpm2.AlgECDSA,
			Hash: tpm2.AlgSHA256,
		}, nil
	default:
		return tpm2.SigScheme{}, fmt.Errorf("algorithm %v not supported", alg)
	}
}

func (k *wrappedKey20) certify(tb tpmBase, handle any, opts CertifyOpts) (*CertificationParameters, error) {
	kAlg, err := k.algorithm()
	if err != nil {
		return nil, fmt.Errorf("unknown algorithm: %v", err)
	}
	ck := certifyingKey{
		handle: k.hnd,
		alg:    kAlg,
	}
	return certifyByKey(tb, handle, ck, opts)
}

func certifyByKey(tb tpmBase, handle any, ck certifyingKey, opts CertifyOpts) (*CertificationParameters, error) {
	t, ok := tb.(*wrappedTPM20)
	if !ok {
		return nil, fmt.Errorf("expected *wrappedTPM20, got %T", tb)
	}
	hnd, ok := handle.(tpmutil.Handle)
	if !ok {
		return nil, fmt.Errorf("expected tpmutil.Handle, got %T", handle)
	}
	scheme, err := sigSchemeFromAlgorithm(ck.alg)
	if err != nil {
		return nil, fmt.Errorf("get signature scheme: %v", err)
	}
	return certify(t.rwc, hnd, ck.handle, opts.QualifyingData, scheme)
}

func (k *wrappedKey20) quote(tb tpmBase, nonce []byte, alg HashAlg, selectedPCRs []int) (*Quote, error) {
	t, ok := tb.(*wrappedTPM20)
	if !ok {
		return nil, fmt.Errorf("expected *wrappedTPM20, got %T", tb)
	}
	return quote20(t.rwc, k.hnd, tpm2.Algorithm(alg), nonce, selectedPCRs)
}

func (k *wrappedKey20) attestationParameters() AttestationParameters {
	return AttestationParameters{
		Public:            k.public,
		CreateData:        k.createData,
		CreateAttestation: k.createAttestation,
		CreateSignature:   k.createSignature,
	}
}

func (k *wrappedKey20) certificationParameters() CertificationParameters {
	return CertificationParameters{
		Public:            k.public,
		CreateAttestation: k.createAttestation,
		CreateSignature:   k.createSignature,
	}
}

func (k *wrappedKey20) sign(tb tpmBase, digest []byte, pub crypto.PublicKey, opts crypto.SignerOpts) ([]byte, error) {
	t, ok := tb.(*wrappedTPM20)
	if !ok {
		return nil, fmt.Errorf("expected *wrappedTPM20, got %T", tb)
	}
	switch p := pub.(type) {
	case *ecdsa.PublicKey:
		return signECDSA(t.rwc, k.hnd, digest, p.Curve)
	case *rsa.PublicKey:
		return signRSA(t.rwc, k.hnd, digest, opts)
	}
	return nil, fmt.Errorf("unsupported signing key type: %T", pub)
}

func signECDSA(rw io.ReadWriter, key tpmutil.Handle, digest []byte, curve elliptic.Curve) ([]byte, error) {
	// https://cs.opensource.google/go/go/+/refs/tags/go1.19.2:src/crypto/ecdsa/ecdsa.go;l=181
	orderBits := curve.Params().N.BitLen()
	orderBytes := (orderBits + 7) / 8
	if len(digest) > orderBytes {
		digest = digest[:orderBytes]
	}
	ret := new(big.Int).SetBytes(digest)
	excess := len(digest)*8 - orderBits
	if excess > 0 {
		ret.Rsh(ret, uint(excess))
	}
	// call ret.FillBytes() here instead of ret.Bytes() to preserve leading zeroes
	// that may have been dropped when converting the digest to an integer
	digest = ret.FillBytes(digest)

	sig, err := tpm2.Sign(rw, key, "", digest, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot sign: %v", err)
	}
	if sig.ECC == nil {
		return nil, fmt.Errorf("expected ECDSA signature, got: %v", sig.Alg)
	}
	return asn1.Marshal(struct {
		R *big.Int
		S *big.Int
	}{sig.ECC.R, sig.ECC.S})
}

func signRSA(rw io.ReadWriter, key tpmutil.Handle, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	h, err := tpm2.HashToAlgorithm(opts.HashFunc())
	if err != nil {
		return nil, fmt.Errorf("incorrect hash algorithm: %v", err)
	}

	scheme := &tpm2.SigScheme{
		Alg:  tpm2.AlgRSASSA,
		Hash: h,
	}

	if pss, ok := opts.(*rsa.PSSOptions); ok {
		if pss.SaltLength != rsa.PSSSaltLengthAuto && pss.SaltLength != len(digest) {
			return nil, fmt.Errorf("PSS salt length %d is incorrect, expected rsa.PSSSaltLengthAuto or %d", pss.SaltLength, len(digest))
		}
		scheme.Alg = tpm2.AlgRSAPSS
	}

	sig, err := tpm2.Sign(rw, key, "", digest, nil, scheme)
	if err != nil {
		return nil, fmt.Errorf("cannot sign: %v", err)
	}
	if sig.RSA == nil {
		return nil, fmt.Errorf("expected RSA signature, got: %v", sig.Alg)
	}
	return sig.RSA.Signature, nil
}

func (k *wrappedKey20) decrypt(tb tpmBase, ctxt []byte) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func (k *wrappedKey20) blobs() ([]byte, []byte, error) {
	return k.public, k.blob, nil
}

func (k *wrappedKey20) algorithm() (Algorithm, error) {
	tpmPub, err := tpm2.DecodePublic(k.public)
	if err != nil {
		return "", fmt.Errorf("decode public key: %v", err)
	}
	switch tpmPub.Type {
	case tpm2.AlgRSA:
		return RSA, nil
	case tpm2.AlgECC:
		return ECDSA, nil
	default:
		return "", fmt.Errorf("unsupported key type: %v", tpmPub.Type)
	}
}
