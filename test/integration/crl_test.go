//go:build integration

package integration

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/letsencrypt/boulder/core"
	"github.com/letsencrypt/boulder/test"
)

// runUpdater executes the crl-updater binary with the -runOnce flag, and
// returns when it completes.
func runUpdater(t *testing.T, configFile string) {
	t.Helper()

	binPath, err := filepath.Abs("bin/crl-updater")
	test.AssertNotError(t, err, "computing crl-updater path")

	c := exec.Command(binPath, "-config", configFile, "-debug-addr", ":8022", "-runOnce")
	err = c.Run()
	test.AssertNotError(t, err, "failed to run crl-updater")
}

// TestCRLPipeline runs an end-to-end test of the crl issuance process, ensuring
// that the correct number of properly-formed and validly-signed CRLs are sent
// to our fake S3 service.
func TestCRLPipeline(t *testing.T) {
	configDir, ok := os.LookupEnv("BOULDER_CONFIG_DIR")
	if !ok {
		t.Fatal("failed to look up test config directory")
	}

	// The crl-updater and crl-storer are not yet deployed in Prod, so only run
	// this test in config-next.
	if !strings.Contains(configDir, "config-next") {
		return
	}

	// Basic setup.
	os.Setenv("DIRECTORY", "http://boulder:4001/directory")
	client, err := makeClient()
	test.AssertNotError(t, err, "creating acme client")
	configFile := path.Join(configDir, "crl-updater.json")

	// Issue a test certificate and save its serial number.
	res, err := authAndIssue(client, nil, []string{random_domain()})
	test.AssertNotError(t, err, "failed to create test certificate")
	cert := res.certs[0]
	serial := core.SerialToString(cert.SerialNumber)

	// Confirm that the cert does not yet show up as revoked in the CRLs.
	runUpdater(t, configFile)
	resp, err := http.Get("http://localhost:7890/query?serial=" + serial)
	test.AssertNotError(t, err, "s3-test-srv GET /query failed")
	test.AssertEquals(t, resp.StatusCode, 404)
	resp.Body.Close()

	// Revoke the certificate.
	err = client.RevokeCertificate(client.Account, cert, client.PrivateKey, 5)
	test.AssertNotError(t, err, "failed to revoke test certificate")

	// Clear the s3-test-srv to prepare for another round of CRLs.
	resp, err = http.Post("http://localhost:7890/clear", "text/plain", nil)
	test.AssertNotError(t, err, "s3-test-srv GET /clear failed")
	test.AssertEquals(t, resp.StatusCode, 200)

	// Confirm that the cert now *does* show up in the CRLs.
	runUpdater(t, configFile)
	resp, err = http.Get("http://localhost:7890/query?serial=" + serial)
	test.AssertNotError(t, err, "s3-test-srv GET /query failed")
	test.AssertEquals(t, resp.StatusCode, 200)

	// Confirm that the revoked certificate entry has the correct reason.
	reason, err := io.ReadAll(resp.Body)
	test.AssertNotError(t, err, "reading revocation reason")
	test.AssertEquals(t, string(reason), "5")
	resp.Body.Close()
}
