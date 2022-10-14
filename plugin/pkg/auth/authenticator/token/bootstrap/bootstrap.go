/*
Copyright 2017 The Kubernetes Authors.

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

/*
Package bootstrap provides a token authenticator for TLS bootstrap secrets.
*/
package bootstrap

import (
	"context"
	"crypto/subtle"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	corev1listers "k8s.io/client-go/listers/core/v1"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	bootstrapsecretutil "k8s.io/cluster-bootstrap/util/secrets"
	bootstraptokenutil "k8s.io/cluster-bootstrap/util/tokens"
	"k8s.io/klog"
)

// TODO: A few methods in this package is copied from other sources. Either
// because the existing functionality isn't exported or because it is in a
// package that shouldn't be directly imported by this packages.

// NewTokenAuthenticator initializes a bootstrap token authenticator.
//
// Lister is expected to be for the "kube-system" namespace.
func NewTokenAuthenticator(lister corev1listers.SecretNamespaceLister) *TokenAuthenticator {
	return &TokenAuthenticator{lister}
}

// TokenAuthenticator authenticates bootstrap tokens from secrets in the API server.
// 1. 解析token是否为bootstrap token, 找到bootstrap token对应的secret内容进行比对。如果token一致，没有过期，且用于验证，则验证成功
type TokenAuthenticator struct {
	lister corev1listers.SecretNamespaceLister
}

// tokenErrorf prints a error message for a secret that has matched a bearer
// token but fails to meet some other criteria.
//
//    tokenErrorf(secret, "has invalid value for key %s", key)
//
func tokenErrorf(s *corev1.Secret, format string, i ...interface{}) {
	format = fmt.Sprintf("Bootstrap secret %s/%s matching bearer token ", s.Namespace, s.Name) + format
	klog.V(3).Infof(format, i...)
}

// AuthenticateToken tries to match the provided token to a bootstrap token secret
// in a given namespace. If found, it authenticates the token in the
// "system:bootstrappers" group and with the "system:bootstrap:(token-id)" username.
//
// All secrets must be of type "bootstrap.kubernetes.io/token". An example secret:
//
//     apiVersion: v1
//     kind: Secret
//     metadata:
//       # Name MUST be of form "bootstrap-token-( token id )".
//       name: bootstrap-token-( token id )
//       namespace: kube-system
//     # Only secrets of this type will be evaluated.
//     type: bootstrap.kubernetes.io/token
//     data:
//       token-secret: ( private part of token )
//       token-id: ( token id )
//       # Required key usage.
//       usage-bootstrap-authentication: true
//       auth-extra-groups: "system:bootstrappers:custom-group1,system:bootstrappers:custom-group2"
//       # May also contain an expiry.
//
// Tokens are expected to be of the form:
//
//     ( token-id ).( token-secret )
//

// 1. 解析token是否为bootstrap token, 找到bootstrap token对应的secret内容进行比对。如果token一致，没有过期，且用于验证，则验证成功
func (t *TokenAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	// 解析bootstrap token, 从中提取tokenID和tokenSecret
	tokenID, tokenSecret, err := bootstraptokenutil.ParseToken(token)
	if err != nil {
		// Token isn't of the correct form, ignore it.
		return nil, false, nil
	}

	//找到bootstrap tokne对应的secret: bootstrap-token-<tokenID>
	secretName := bootstrapapi.BootstrapTokenSecretPrefix + tokenID
	secret, err := t.lister.Get(secretName)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(3).Infof("No secret of name %s to match bootstrap bearer token", secretName)
			return nil, false, nil
		}
		return nil, false, err
	}
	// secret是否准备删除
	if secret.DeletionTimestamp != nil {
		tokenErrorf(secret, "is deleted and awaiting removal")
		return nil, false, nil
	}

	// 当前secret是否bootstrap token secret
	if string(secret.Type) != string(bootstrapapi.SecretTypeBootstrapToken) || secret.Data == nil {
		tokenErrorf(secret, "has invalid type, expected %s.", bootstrapapi.SecretTypeBootstrapToken)
		return nil, false, nil
	}

	//提取secret中的bootstrap token secret
	ts := bootstrapsecretutil.GetData(secret, bootstrapapi.BootstrapTokenSecretKey)
	// 比较传过来的token secret和secret中保存的token secret是否一致
	if subtle.ConstantTimeCompare([]byte(ts), []byte(tokenSecret)) != 1 {
		tokenErrorf(secret, "has invalid value for key %s, expected %s.", bootstrapapi.BootstrapTokenSecretKey, tokenSecret)
		return nil, false, nil
	}

	// 比较传过来的token id和secret中保存的token id是否一致
	id := bootstrapsecretutil.GetData(secret, bootstrapapi.BootstrapTokenIDKey)
	if id != tokenID {
		tokenErrorf(secret, "has invalid value for key %s, expected %s.", bootstrapapi.BootstrapTokenIDKey, tokenID)
		return nil, false, nil
	}

	// bootstrap token是否过期
	if bootstrapsecretutil.HasExpired(secret, time.Now()) {
		// logging done in isSecretExpired method.
		return nil, false, nil
	}

	// 判断当前bootstrap token是否用于验证使用
	if bootstrapsecretutil.GetData(secret, bootstrapapi.BootstrapTokenUsageAuthentication) != "true" {
		tokenErrorf(secret, "not marked %s=true.", bootstrapapi.BootstrapTokenUsageAuthentication)
		return nil, false, nil
	}

	groups, err := bootstrapsecretutil.GetGroups(secret)
	if err != nil {
		tokenErrorf(secret, "has invalid value for key %s: %v.", bootstrapapi.BootstrapTokenExtraGroupsKey, err)
		return nil, false, nil
	}

	return &authenticator.Response{
		User: &user.DefaultInfo{
			Name:   bootstrapapi.BootstrapUserPrefix + string(id), //用户名为"system:bootstrap:<token id>"
			Groups: groups,
		},
	}, true, nil
}
