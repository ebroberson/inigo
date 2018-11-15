package cell_test

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"code.cloudfoundry.org/archiver/extractor/test_helper"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/cfhttp"
	"code.cloudfoundry.org/durationjson"
	"code.cloudfoundry.org/guardian/gqt/runner"
	"code.cloudfoundry.org/inigo/fixtures"
	"code.cloudfoundry.org/inigo/helpers"
	repconfig "code.cloudfoundry.org/rep/cmd/rep/config"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Evacuation", func() {
	var (
		runtime ifrit.Process

		cellAID            string
		cellARepAddr       string
		cellARepSecureAddr string

		cellBID            string
		cellBRepAddr       string
		cellBRepSecureAddr string

		cellARepRunner *ginkgomon.Runner
		cellBRepRunner *ginkgomon.Runner

		cellA ifrit.Process
		cellB ifrit.Process

		processGuid    string
		lrp            *models.DesiredLRP
		cellPortsStart uint16

		httpClient *http.Client
	)

	BeforeEach(func() {
		processGuid = helpers.GenerateGuid()

		fileServer, fileServerStaticDir := componentMaker.FileServer()

		By("restarting the bbs with smaller convergeRepeatInterval")
		ginkgomon.Interrupt(bbsProcess)
		bbsProcess = ginkgomon.Invoke(componentMaker.BBS(
			overrideConvergenceRepeatInterval,
		))

		runtime = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{"router", componentMaker.Router()},
			{"file-server", fileServer},
			{"auctioneer", componentMaker.Auctioneer()},
			{"route-emitter", componentMaker.RouteEmitter()},
		}))

		cellAID = "cell-a"
		cellBID = "cell-b"

		var err error
		cellPortsStart, err = componentMaker.PortAllocator().ClaimPorts(4)
		Expect(err).NotTo(HaveOccurred())

		tlscfg, err := cfhttp.NewTLSConfig(
			componentMaker.RepSSLConfig().ClientCert,
			componentMaker.RepSSLConfig().ClientKey,
			componentMaker.RepSSLConfig().CACert,
		)
		Expect(err).NotTo(HaveOccurred())

		httpClient = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlscfg,
			},
		}

		cellARepAddr = fmt.Sprintf("0.0.0.0:%d", cellPortsStart)
		cellARepSecureAddr = fmt.Sprintf("0.0.0.0:%d", cellPortsStart+1)
		cellBRepAddr = fmt.Sprintf("0.0.0.0:%d", cellPortsStart+2)
		cellBRepSecureAddr = fmt.Sprintf("0.0.0.0:%d", cellPortsStart+3)

		cellARepRunner = componentMaker.RepN(0,
			func(config *repconfig.RepConfig) {
				config.CellID = cellAID
				config.ListenAddr = cellARepAddr
				config.ListenAddrSecurable = cellARepSecureAddr
				config.EvacuationTimeout = durationjson.Duration(30 * time.Second)
			},
		)

		cellBRepRunner = componentMaker.RepN(1,
			func(config *repconfig.RepConfig) {
				config.CellID = cellBID
				config.ListenAddr = cellBRepAddr
				config.ListenAddrSecurable = cellBRepSecureAddr
				config.EvacuationTimeout = durationjson.Duration(30 * time.Second)
			})

		test_helper.CreateZipArchive(
			filepath.Join(fileServerStaticDir, "lrp.zip"),
			fixtures.GoServerApp(),
		)

		lrp = helpers.DefaultLRPCreateRequest(componentMaker.Addresses(), processGuid, "log-guid", 1)
	})

	JustBeforeEach(func() {
		cellA = ginkgomon.Invoke(cellARepRunner)
		cellB = ginkgomon.Invoke(cellBRepRunner)
	})

	AfterEach(func() {
		helpers.StopProcesses(runtime, cellA, cellB)
	})

	It("handles evacuation", func() {
		By("desiring an LRP")
		err := bbsClient.DesireLRP(logger, lrp)
		Expect(err).NotTo(HaveOccurred())

		By("running an actual LRP instance")
		Eventually(helpers.LRPStatePoller(logger, bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))
		Eventually(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses().Router, helpers.DefaultHost)).Should(Equal(http.StatusOK))

		actualLRPGroup, err := bbsClient.ActualLRPGroupByProcessGuidAndIndex(logger, processGuid, 0)
		Expect(err).NotTo(HaveOccurred())

		actualLRP, isEvacuating, err := actualLRPGroup.Resolve()
		Expect(err).NotTo(HaveOccurred())
		Expect(isEvacuating).To(BeFalse())

		var evacuatingRepPort uint16
		var evacuatingRepRunner *ginkgomon.Runner

		switch actualLRP.CellId {
		case cellAID:
			evacuatingRepRunner = cellARepRunner
			evacuatingRepPort = cellPortsStart
		case cellBID:
			evacuatingRepRunner = cellBRepRunner
			evacuatingRepPort = cellPortsStart + 2
		default:
			panic("what? who?")
		}

		By("posting the evacuation endpoint")
		// Rep admin endpoint verifies and validate 127.0.0.1 for IP SAN
		resp, err := httpClient.Post(fmt.Sprintf("https://127.0.0.1:%d/evacuate", evacuatingRepPort), "text/html", nil)
		Expect(err).NotTo(HaveOccurred())
		resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusAccepted))

		By("staying routable so long as its rep is alive")
		Eventually(func() int {
			Expect(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses().Router, helpers.DefaultHost)()).To(Equal(http.StatusOK))
			return evacuatingRepRunner.ExitCode()
		}).Should(Equal(0))

		By("running immediately after the rep exits and is routable")
		Expect(helpers.LRPStatePoller(logger, bbsClient, processGuid, nil)()).To(Equal(models.ActualLRPStateRunning))
		Consistently(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses().Router, helpers.DefaultHost)).Should(Equal(http.StatusOK))
	})

	Context("when garden Destroy hangs", func() {
		BeforeEach(func() {
			replaceGrootFSWithHangingVersion()
			cellARepRunner = componentMaker.RepN(0,
				func(config *repconfig.RepConfig) {
					config.CellID = cellAID
					config.ListenAddr = cellARepAddr
					config.ListenAddrSecurable = cellARepSecureAddr
					config.GracefulShutdownInterval = 1 // 1 nanosecond otherwise a 0 is treated as omitted value
				},
			)
		})

		JustBeforeEach(func() {
			// kill cell-b to simplify the test. otherwise, we will have to figure
			// out which cell to evacuate
			ginkgomon.Kill(cellB)
		})

		It("shuts down gracefully after the evacuation timeout", func() {
			time.Sleep(time.Minute)

			err := bbsClient.DesireLRP(logger, lrp)
			Expect(err).NotTo(HaveOccurred())

			By("running an actual LRP instance")
			Eventually(helpers.LRPStatePoller(logger, bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))

			By("posting the evacuation endpoint")
			// Rep admin endpoint verifies and validate 127.0.0.1 for IP SAN
			resp, err := httpClient.Post(fmt.Sprintf("https://127.0.0.1:%d/evacuate", cellPortsStart), "text/html", nil)
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusAccepted))

			factory := componentMaker.RepClientFactory()
			addr := fmt.Sprintf("https://%s.cell.service.cf.internal:%d", cellAID, cellPortsStart)
			secureAddr := fmt.Sprintf("https://%s.cell.service.cf.internal:%d", cellAID, cellPortsStart+1)
			// NewClientFactory <<- tls
			client, err := factory.CreateClient(addr, secureAddr)
			Expect(err).NotTo(HaveOccurred())

			group, err := bbsClient.ActualLRPGroupByProcessGuidAndIndex(logger, lrp.ProcessGuid, 0)
			Expect(err).NotTo(HaveOccurred())

			// the following requests will hang since garden is stuck trying to destroy the containers
			go func() {
				for i := 0; i < 100; i++ {
					err := client.StopLRPInstance(logger, group.Instance.ActualLRPKey, group.Instance.ActualLRPInstanceKey)
					if err != nil {
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
			}()

			// hanging http requests shouldn't prevent the process from exiting
			Eventually(cellA.Wait(), 10*time.Second).Should(Receive())
		})
	})
})

func replaceGrootFSWithHangingVersion() {
	f, err := ioutil.TempFile(os.TempDir(), "image_plugin")
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Chmod(0755)).To(Succeed())
	path := filepath.Join(os.Getenv("GROOTFS_BINPATH"), "grootfs")
	os.Remove(fmt.Sprintf("/tmp/image_plugin_sleep_%d", GinkgoParallelNode()))
	fmt.Fprintf(f, `#!/usr/bin/env bash
echo $(date +%%s) "$@" >> /tmp/image_plugin_trace
lock_file=/tmp/image_plugin_sleep_%d
# sleep on delete of non healthcheck container and only the first time
if [[ "x$3" == "xdelete" && ! ( "$4" =~ .*healthcheck.* ) && ! -f $lock_file ]]; then
  touch $lock_file
  sleep 10
fi
%s "$@"
`, GinkgoParallelNode(), path)
	Expect(f.Close()).To(Succeed())
	ginkgomon.Interrupt(gardenProcess)
	gardenProcess = ginkgomon.Invoke(componentMaker.Garden(func(config *runner.GdnRunnerConfig) {
		config.ImagePluginBin = f.Name()
		config.PrivilegedImagePluginBin = f.Name()
	}))
}
