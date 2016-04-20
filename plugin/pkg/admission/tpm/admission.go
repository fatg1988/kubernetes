/*
Copyright 2014 The Kubernetes Authors All rights reserved.
Copyright 2015 CoreOS, Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package admit

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/admission"
	"k8s.io/kubernetes/pkg/api"
        apierrors "k8s.io/kubernetes/pkg/api/errors"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/util/tpm"
)

const untrusted string = "com.coreos.tpm/untrusted"

func init() {
	admission.RegisterPlugin("TPMAdmit", func(client clientset.Interface, config io.Reader) (admission.Interface, error) {
		return NewTPMAdmit(client, config), nil
	})	
}

// TPMAdmit is an implementation of admission.Interface which performs TPM-based validation of the request
type tpmAdmit struct{
	handler tpm.TPMHandler
	client clientset.Interface
	pcrconfig string
	pcrconfigdir string
	allowunknown bool
}

// Flag a node as untrusted
func invalidateNode(node *api.Node) {
	if node.ObjectMeta.Annotations == nil {
		node.ObjectMeta.Annotations = make(map[string]string)
	}
	node.ObjectMeta.Annotations[untrusted] = "true"
}

func (t *tpmAdmit) verifyNode(node *api.Node) (err error) {
	address, err := nodeutil.GetNodeHostIP(node)
	if err != nil{
		return err
	}
	host := fmt.Sprintf("%s:23179", address.String())
	tpmdata, err := t.handler.Get(host, t.allowunknown)
	if err != nil {
		glog.Errorf("Unable to obtain TPM data for node %s: %v", address.String(), err)
		invalidateNode(node)
		return nil
	}
	quote, log, err := tpm.Quote(tpmdata)
	if err != nil {
		glog.Errorf("Unable to obtain TPM quote for node %s: %v", address.String(), err)
		invalidateNode(node)
		return nil
	}

	pcrconfigs := make([]map[string]tpm.PCRConfig, 0)
	if t.pcrconfig != "" {
		pcrdata := make(map[string]tpm.PCRConfig)
		pcrconfig, err := ioutil.ReadFile(t.pcrconfig)
		if err != nil {
			glog.Errorf("Unable to read valid PCR configuration %s: %v", t.pcrconfig, err)
		}
		err = json.Unmarshal(pcrconfig, &pcrdata)
		if err != nil {
			glog.Errorf("Unable to parse valid PCR configuration %s: %v", t.pcrconfig, err)
		}
		for pcr, _ := range(pcrdata) {
			pcrtmp := pcrdata[pcr]
			pcrtmp.Source = t.pcrconfig
			pcrdata[pcr] = pcrtmp
		}
		pcrconfigs = append(pcrconfigs, pcrdata)
	} else  if t.pcrconfigdir != "" {
		err = filepath.Walk(t.pcrconfigdir, func(path string, f os.FileInfo, err error) error {
			if f.IsDir() {
				return nil
			}
			pcrconfig, err := ioutil.ReadFile(path)
			if err != nil {
				glog.Errorf("Unable to read PCR configuration %s: %v", path, err)
				return err
			}
			pcrdata := make(map[string]tpm.PCRConfig)
			err = json.Unmarshal(pcrconfig, &pcrdata)
			if err != nil {
				glog.Errorf("Unable to parse valid PCR configuration %s: %v", path, err)
				return err
			}
			for pcr, _ := range(pcrdata) {
				pcrtmp := pcrdata[pcr]
				pcrtmp.Source = path
				pcrdata[pcr] = pcrtmp
			}
			pcrconfigs = append(pcrconfigs, pcrdata)
			return nil
		})
	} else {
		pcrconfigs, err = t.handler.GetPolicies()
		if err != nil {
			glog.Errorf("Unable to obtain PCR configuration: %v", err)
			invalidateNode(node)
			return nil
		}
	}

	err = tpm.ValidateLog(log, quote)
	if err != nil {
		glog.Errorf("TPM event log does not match quote for node %s", address.String())
		invalidateNode(node)
		return nil
	}

	logstate, err := tpm.ValidatePCRs(log, quote, pcrconfigs)
	jsonlog, _ := json.Marshal(logstate)
	if node.ObjectMeta.Annotations == nil {
		node.ObjectMeta.Annotations = make(map[string]string)
	}
	node.ObjectMeta.Annotations["com.coreos.tpm/logstate"] = string(jsonlog)
	if err != nil {
		glog.Errorf("TPM quote PCRs don't validate for node %s", address.String())
		invalidateNode(node)
		return nil
	}

	glog.Errorf("TPM quote for node %s validates", address.String())
	node.ObjectMeta.Annotations[untrusted] = "false"
	return nil
}

func (t *tpmAdmit) Admit(a admission.Attributes) (err error) {
	if a.GetKind() != api.Kind("Node") {
		return nil
	}

	node, ok := a.GetObject().(*api.Node)
	if !ok {
		glog.Errorf("Object is %v", a.GetObject())
		return apierrors.NewBadRequest("Resource was marked with kind Node but was unable to be converted")
	}

	if a.GetOperation() == admission.Create {
		err = t.verifyNode(node)
	} else if a.GetOperation() == admission.Update {
		old, err := t.client.Core().Nodes().Get(node.ObjectMeta.Name)
		if err == nil && old.ObjectMeta.Annotations[untrusted] == "true" {
			err = t.verifyNode(node)
		}
	}

	if err != nil {
		return admission.NewForbidden(a, err)
	}

	return nil
}

func (tpmAdmit) Handles(operation admission.Operation) bool {
	if operation == admission.Create || operation == admission.Update {
		return true
	}
	return false
}

func reverify(t *tpmAdmit, delay int) {
	for range time.Tick(time.Second * time.Duration(delay)) {
		nodes, err := t.client.Core().Nodes().List(api.ListOptions{})
		if err != nil {
			continue
		}
		for _, node := range nodes.Items {
			state := node.ObjectMeta.Annotations[untrusted]
			t.verifyNode(&node)
			newstate := node.ObjectMeta.Annotations[untrusted]
			if newstate != state {
				newnode, err := t.client.Core().Nodes().Get(node.Name)
				if err != nil {
					glog.Errorf("Unable to obtain node state for %s: %v", node.Name, err)
					continue
				}
				if newnode.ObjectMeta.Annotations == nil {
					newnode.ObjectMeta.Annotations = make(map[string]string)
				}
				newnode.ObjectMeta.Annotations[untrusted] = newstate
				newnode, err = t.client.Core().Nodes().Update(newnode)
				if err != nil {
					glog.Errorf("Unable to update node state for %s: %v", node.Name, err)
					continue
				}
			}
		}
	}
}

// NewTPMAdmit creates a new TPMAdmit handler
func NewTPMAdmit(c clientset.Interface, config io.Reader) admission.Interface {
	var tpmhandler tpm.TPMHandler
	var pcrconfig string
	var pcrconfigdir string
	var allowunknown bool
	tpmhandler.Setup()

	jsondata, err := ioutil.ReadAll(config)
	if err != nil {
		return nil
	}
	var configdata map[string]interface{}
	err = json.Unmarshal(jsondata, &configdata)
	if err != nil {
		return nil
	}
	if configdata["tpmadmit.pcrconfig"] != nil {
		pcrconfig = configdata["tpmadmit.pcrconfig"].(string)
	}
	if configdata["tpmadmit.pcrconfigdir"] != nil {
		pcrconfigdir = configdata["tpmadmit.pcrconfigdir"].(string)
	}
	if configdata["tpmadmit.allowunknown"] != nil {
		allowunknown = configdata["tpmadmit.allowunknown"].(bool)
	}
	tpmadmit := &tpmAdmit{
		handler: tpmhandler,
		client: c,
		pcrconfig: pcrconfig,
		pcrconfigdir: pcrconfigdir,
		allowunknown: allowunknown,
	}
	if configdata["tpmadmit.recurring"] != nil {
		go reverify(tpmadmit, int(configdata["tpmadmit.recurring"].(float64)))
	}
	return tpmadmit
}
