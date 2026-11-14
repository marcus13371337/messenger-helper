package meta

import (
	"encoding/base64"
	"encoding/json"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

type attachmentRef struct {
	URL  string `json:"url,omitempty"`
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`

	DirectPath    string `json:"direct_path,omitempty"`
	MediaKey      []byte `json:"media_key,omitempty"`
	FileSHA256    []byte `json:"file_sha256,omitempty"`
	FileEncSHA256 []byte `json:"file_enc_sha256,omitempty"`
	MediaType     string `json:"media_type,omitempty"`
}

func (r attachmentRef) encode() string {
	raw, _ := json.Marshal(r)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeRef(ref string) (attachmentRef, error) {
	var r attachmentRef
	raw, err := base64.RawURLEncoding.DecodeString(ref)
	if err == nil {
		err = json.Unmarshal(raw, &r)
	}
	if err != nil || (r.URL == "" && r.DirectPath == "") {
		return r, helper.Errorf(helper.CodeBadRequest, "That attachment ref did not come from a message.")
	}
	return r, nil
}
