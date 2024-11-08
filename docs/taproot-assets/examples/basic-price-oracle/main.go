// This example demonstrates a basic RPC price oracle server that implements the
// QueryAssetRates RPC method. The server listens on localhost:8095 and returns
// the asset rates for a given transaction type, subject asset, and payment
// asset.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/lightninglabs/taproot-assets/rfqmath"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	oraclerpc "github.com/lightninglabs/taproot-assets/taprpc/priceoraclerpc"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	// serviceListenAddress is the listening address of the service.
	serviceListenAddress = "localhost:8095"
)

// setupLogger sets up the logger to write logs to a file.
func setupLogger() {
	// Create a log file.
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	file, err := os.OpenFile("basic-price-oracle-example.log", flags, 0666)
	if err != nil {
		logrus.Fatalf("Failed to open log file: %v", err)
	}

	// Create a multi-writer to write to both stdout and the file.
	multiWriter := io.MultiWriter(os.Stdout, file)

	// Set the output of logrus to the multi-writer.
	logrus.SetOutput(multiWriter)

	// Set the log level (optional).
	logrus.SetLevel(logrus.DebugLevel)

	// Set the log format (optional).
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
}

// RpcPriceOracleServer is a basic example RPC price oracle server.
type RpcPriceOracleServer struct {
	oraclerpc.UnimplementedPriceOracleServer
}

// isSupportedSubjectAsset returns true if the given subject asset is supported
// by the price oracle, and false otherwise.
func isSupportedSubjectAsset(subjectAsset *oraclerpc.AssetSpecifier) bool {
	// Ensure that the subject asset is set.
	if subjectAsset == nil {
		logrus.Info("Subject asset is not set (nil)")
		return false
	}

	supportedAssetIdStr := "7b4336d33b019df9438e586f83c587ca00fa65602497b9" +
		"3ace193e9ce53b1a67"
	supportedAssetIdBytes, err := hex.DecodeString(supportedAssetIdStr)
	if err != nil {
		fmt.Println("Error decoding supported asset hex string:", err)
		return false
	}

	// Check the subject asset bytes if set.
	subjectAssetIdBytes := subjectAsset.GetAssetId()
	if len(subjectAssetIdBytes) > 0 {
		logrus.Infof("Subject asset ID bytes populated: %x",
			supportedAssetIdBytes)
		return bytes.Equal(supportedAssetIdBytes, subjectAssetIdBytes)
	}

	subjectAssetIdStr := subjectAsset.GetAssetIdStr()
	if len(subjectAssetIdStr) > 0 {
		logrus.Infof("Subject asset ID str populated: %s",
			supportedAssetIdStr)
		return subjectAssetIdStr == supportedAssetIdStr
	}

	logrus.Infof("Subject asset ID not set")
	return false
}

// getAssetRates returns the asset rates for a given transaction type and
// subject asset max amount.
func getAssetRates(transactionType oraclerpc.TransactionType,
	subjectAssetMaxAmount uint64) (oraclerpc.AssetRates, error) {

	// Determine the rate based on the transaction type.
	var subjectAssetRate rfqmath.BigIntFixedPoint
	if transactionType == oraclerpc.TransactionType_PURCHASE {
		// As an example, the purchase rate is $42,000 per BTC. To
		// increase precision, we represent this as 42 billion
		// taproot asset units per BTC. Therefore, we scale the
		// $42,000 per BTC rate by a factor of 10^6.
		subjectAssetRate = rfqmath.FixedPointFromUint64[rfqmath.BigInt](
			42_000, 6,
		)
	} else {
		// Our example sell rate will be lower at $40,000 per BTC. This
		// rate will be represented as 40 billion taproot asset units
		// per BTC. Therefore, we scale the $40,000 per BTC rate by a
		// factor of 10^6.
		subjectAssetRate = rfqmath.FixedPointFromUint64[rfqmath.BigInt](
			40_000, 6,
		)
	}

	// Set the rate expiry to 5 minutes by default.
	expiry := time.Now().Add(5 * time.Minute).Unix()

	// If the subject asset max amount is greater than 100,000, set the rate
	// expiry to 1 minute.
	if subjectAssetMaxAmount > 100_000 {
		expiry = time.Now().Add(1 * time.Minute).Unix()
	}

	// Marshal subject asset rate to RPC format.
	rpcSubjectAssetToBtcRate, err := oraclerpc.MarshalBigIntFixedPoint(
		subjectAssetRate,
	)
	if err != nil {
		return oraclerpc.AssetRates{}, err
	}

	// Marshal payment asset rate to RPC format.
	rpcPaymentAssetToBtcRate, err := oraclerpc.MarshalBigIntFixedPoint(
		rfqmsg.MilliSatPerBtc,
	)
	if err != nil {
		return oraclerpc.AssetRates{}, err
	}

	return oraclerpc.AssetRates{
		SubjectAssetRate: rpcSubjectAssetToBtcRate,
		PaymentAssetRate: rpcPaymentAssetToBtcRate,
		ExpiryTimestamp:  uint64(expiry),
	}, nil
}

// QueryAssetRates queries the asset rates for a given transaction type, subject
// asset, and payment asset. An asset rate is the number of asset units per
// BTC.
//
// Example use case:
//
// Alice is trying to pay an invoice by spending an asset. Alice therefore
// requests that Bob (her asset channel counterparty) purchase the asset from
// her. Bob's payment, in BTC, will pay the invoice.
//
// Alice requests a bid quote from Bob. Her request includes an asset rates hint
// (ask). Alice obtains the asset rates hint by calling this endpoint. She sets:
// - `SubjectAsset` to the asset she is trying to sell.
// - `SubjectAssetMaxAmount` to the max channel asset outbound.
// - `PaymentAsset` to BTC.
// - `TransactionType` to SALE.
// - `AssetRateHint` to nil.
//
// Bob calls this endpoint to get the bid quote asset rates that he will send as
// a response to Alice's request. He sets:
// - `SubjectAsset` to the asset that Alice is trying to sell.
// - `SubjectAssetMaxAmount` to the value given in Alice's quote request.
// - `PaymentAsset` to BTC.
// - `TransactionType` to PURCHASE.
// - `AssetRateHint` to the value given in Alice's quote request.
func (p *RpcPriceOracleServer) QueryAssetRates(_ context.Context,
	req *oraclerpc.QueryAssetRatesRequest) (
	*oraclerpc.QueryAssetRatesResponse, error) {

	// Ensure that the payment asset is BTC. We only support BTC as the
	// payment asset in this example.
	if !oraclerpc.IsAssetBtc(req.PaymentAsset) {
		logrus.Infof("Payment asset is not BTC: %v", req.PaymentAsset)

		return &oraclerpc.QueryAssetRatesResponse{
			Result: &oraclerpc.QueryAssetRatesResponse_Error{
				Error: &oraclerpc.QueryAssetRatesErrResponse{
					Message: "unsupported payment asset, " +
						"only BTC is supported",
				},
			},
		}, nil
	}

	// Ensure that the subject asset is set.
	if req.SubjectAsset == nil {
		logrus.Info("Subject asset is not set")
		return nil, fmt.Errorf("subject asset is not set")
	}

	// Ensure that the subject asset is supported.
	if !isSupportedSubjectAsset(req.SubjectAsset) {
		logrus.Infof("Unsupported subject asset ID str: %v\n",
			req.SubjectAsset)

		return &oraclerpc.QueryAssetRatesResponse{
			Result: &oraclerpc.QueryAssetRatesResponse_Error{
				Error: &oraclerpc.QueryAssetRatesErrResponse{
					Message: "unsupported subject asset",
				},
			},
		}, nil
	}

	// Determine which asset rate to return.
	var (
		assetRates oraclerpc.AssetRates
		err        error
	)

	if req.AssetRatesHint != nil {
		// If the asset rates hint is provided, return it as the asset
		// rate. In doing so, we effectively accept the asset rates
		// proposed by our peer.
		logrus.Info("Suggested asset to BTC rate provided, " +
			"returning rate as accepted rate")

		assetRates = oraclerpc.AssetRates{
			SubjectAssetRate: req.AssetRatesHint.SubjectAssetRate,
			PaymentAssetRate: req.AssetRatesHint.PaymentAssetRate,
			ExpiryTimestamp:  req.AssetRatesHint.ExpiryTimestamp,
		}
	} else {
		// If an asset rates hint is not provided, fetch asset rates
		// from our internal system.
		logrus.Info("Suggested asset to BTC rate not provided, " +
			"querying internal system for rate")

		assetRates, err = getAssetRates(
			req.TransactionType, req.SubjectAssetMaxAmount,
		)
		if err != nil {
			return nil, err
		}
	}

	logrus.Infof("QueryAssetRates returning rates (subject_asset_rate=%v, "+
		"payment_asset_rate=%v)", assetRates.SubjectAssetRate,
		assetRates.PaymentAssetRate)

	return &oraclerpc.QueryAssetRatesResponse{
		Result: &oraclerpc.QueryAssetRatesResponse_Ok{
			Ok: &oraclerpc.QueryAssetRatesOkResponse{
				AssetRates: &assetRates,
			},
		},
	}, nil
}

// startService starts the given RPC server and blocks until the server is
// shut down.
func startService(grpcServer *grpc.Server) error {
	serviceAddr := fmt.Sprintf("rfqrpc://%s", serviceListenAddress)
	logrus.Infof("Starting RPC price oracle service at address: %s\n",
		serviceAddr)

	server := RpcPriceOracleServer{}
	oraclerpc.RegisterPriceOracleServer(grpcServer, &server)
	grpcListener, err := net.Listen("tcp", serviceListenAddress)
	if err != nil {
		return fmt.Errorf("RPC server unable to listen on %s",
			serviceListenAddress)
	}
	return grpcServer.Serve(grpcListener)
}

// Generate a self-signed TLS certificate and private key.
func generateSelfSignedCert() (tls.Certificate, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	keyUsage := x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature
	extKeyUsage := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"basic-price-oracle"},
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(24 * time.Hour), // Valid for 1 day

		KeyUsage:              keyUsage,
		ExtKeyUsage:           extKeyUsage,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader, &template, &template, &privateKey.PublicKey,
		privateKey,
	)
	if err != nil {
		return tls.Certificate{}, err
	}

	privateKeyBits, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: certDER},
	)
	keyPEM := pem.EncodeToMemory(
		&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKeyBits},
	)

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tlsCert, nil
}

func main() {
	setupLogger()

	// Start the mock RPC price oracle service.
	//
	// Generate self-signed certificate. This allows us to use TLS for the
	// gRPC server.
	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatalf("Failed to generate TLS certificate: %v", err)
	}

	// Create the gRPC server with TLS
	transportCredentials := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})
	backendService := grpc.NewServer(grpc.Creds(transportCredentials))

	err = startService(backendService)
	if err != nil {
		log.Fatalf("Start service error: %v", err)
	}

	backendService.GracefulStop()
}
