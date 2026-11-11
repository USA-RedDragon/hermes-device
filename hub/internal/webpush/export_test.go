package webpush

import "crypto/ecdh"

// EncryptWith runs the payload encryption with a fixed salt and sender key.
func EncryptWith(sub Subscription, plaintext, salt, senderPriv []byte) ([]byte, error) {
	uaPub, auth, err := sub.keys()
	if err != nil {
		return nil, err
	}
	as, err := ecdh.P256().NewPrivateKey(senderPriv)
	if err != nil {
		return nil, err
	}
	return encrypt(uaPub, auth, plaintext, salt, as)
}

// AllowAnyEndpoint makes Send skip ValidEndpoint so tests can use httptest.
func AllowAnyEndpoint(v *VAPID) { v.allowAnyEndpoint = true }
