package vaultd

// 星财富通道的单元测试。
//
// # 只测「可信部分」
//
// 签名函数的实现（RSA-SHA256 over PEM）是真加密操作，可以且必须验证。
// endpoint / header 名是推测的，测它们等于测自己的假设 —— 没意义。
// 所以这里只断言：
//   - PEM 解析成功/失败的边界（PKCS#1 / PKCS#8 / 垃圾输入）
//   - 签名能被对应公钥验证通过（这是唯一重要的性质）
//   - 缺凭证时构造返回 error 而不是 panic
//   - Health 返回明确的「未验证」错误，不会给出假绿灯

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
)

// genRSAKeyPEM 生成一把测试用 RSA 私钥，返回私钥与对应的 PKCS#8/PKCS#1 PEM。
func genTestKeyPEM(t *testing.T, pkcs8 bool) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 私钥失败: %v", err)
	}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
		}
		return k, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	der := x509.MarshalPKCS1PrivateKey(k)
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

// TestFosunSignerSignsVerifiably 签名必须能被公钥验过 —— 这是唯一重要的性质。
func TestFosunSignerSignsVerifiably(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		name := "PKCS#1"
		if pkcs8 {
			name = "PKCS#8"
		}
		t.Run(name, func(t *testing.T) {
			key, pemText := genTestKeyPEM(t, pkcs8)
			s, err := newFosunPEMSigner(pemText)
			if err != nil {
				t.Fatalf("newFosunPEMSigner: %v", err)
			}

			const method, path = "POST", "/openapi/order"
			body := []byte(`{"a":1}`)
			headers, err := s.Sign(method, path, body)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}

			sigB64, ok := headers["X-FOSUN-SIGN"]
			if !ok || sigB64 == "" {
				t.Fatalf("缺少 X-FOSUN-SIGN，headers=%v", headers)
			}
			if headers["X-FOSUN-TIMESTAMP"] == "" {
				t.Error("缺少 X-FOSUN-TIMESTAMP")
			}

			sig, err := base64.StdEncoding.DecodeString(sigB64)
			if err != nil {
				t.Fatalf("签名不是合法 base64: %v", err)
			}
			prehash := method + "\n" + path + "\n" + string(body)
			sum := sha256.Sum256([]byte(prehash))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
				t.Fatalf("签名验不过（说明签名原文或算法与实现不一致）: %v", err)
			}
		})
	}
}

// TestFosunSignerRejectsGarbage 非 PEM 输入应报错。
func TestFosunSignerRejectsGarbage(t *testing.T) {
	for name, in := range map[string]string{
		"空串":   "",
		"纯文本":  "not a pem",
		"错误类型": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	} {
		if _, err := newFosunPEMSigner(in); err == nil {
			t.Errorf("%s 应返回 error", name)
		}
	}
}

// TestNewFosunChannelRequiresCredentials 缺凭证返回 error，不 panic。
func TestNewFosunChannelRequiresCredentials(t *testing.T) {
	if _, err := NewFosunChannel(FosunConfig{}); err == nil {
		t.Error("全空配置应返回 error")
	}
	if _, err := NewFosunChannel(FosunConfig{ClientPrivateKey: "x", AccountID: ""}); err == nil {
		t.Error("缺 account_id 应返回 error")
	}
	// 有 PEM 但内容非法 → 也要 error（不能等到下单时才炸）。
	if _, err := NewFosunChannel(FosunConfig{
		ClientPrivateKey: "not-a-pem", AccountID: "acc",
	}); err == nil {
		t.Error("非法 PEM 应在构造期报错")
	}
}

// TestFosunHealthIsExplicitError Health 必须给明确红灯，不能假绿。
func TestFosunHealthIsExplicitError(t *testing.T) {
	_, pemText := genTestKeyPEM(t, true)
	ch, err := NewFosunChannel(FosunConfig{
		ClientPrivateKey: pemText,
		AccountID:        "acc-1",
	})
	if err != nil {
		t.Fatalf("NewFosunChannel: %v", err)
	}
	herr := ch.Health()
	if herr == nil {
		t.Fatal("Health 应返回「未接入验证」错误，不能返回 nil（假绿灯比红灯危险）")
	}
	if !strings.Contains(herr.Error(), "未接入验证") {
		t.Errorf("Health 错误应明确说明未接入验证，实际：%v", herr)
	}
}
