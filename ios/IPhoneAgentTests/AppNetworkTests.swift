import CryptoKit
import Network
import XCTest
@testable import IPhoneAgent

final class AppNetworkTests: XCTestCase {
    private let master = Data(0..<32)
    private let clientNonce = Data(32..<48)
    private let welcome = "82303132333435363738393a3b3c3d3e3ff12810704765c7823f4c222f4179e4bf"
    private let command = "2001000000b4c696064217e89d2bc9f7422d4936d4120ced5546b029950f"
    private let status = "20010000001ddac39e55d5c4012926993ac9c892601e0bc237f50aeea2518671687d"

    private func data(_ hex: String) -> Data {
        Data(stride(from: 0, to: hex.count, by: 2).map {
            let start = hex.index(hex.startIndex, offsetBy: $0)
            return UInt8(hex[start..<hex.index(start, offsetBy: 2)], radix: 16)!
        })
    }
    private func session() throws -> AppNetworkCrypto {
        try AppNetworkCrypto(master: master, clientNonce: clientNonce, welcome: data(welcome))
    }

    // Shared vectors are also checked by the firmware's independent mbedtls implementation.
    func testFirmwareCommandAndStatusVectors() throws {
        var crypto = try session()
        XCTAssertEqual(try crypto.seal(Data("c123 ping".utf8)), data(command))
        XCTAssertEqual(try crypto.open(data(status)), Data("s{\"usb\":\"ok\"}".utf8))
    }

    func testStatusCannotBeReplayedOrReordered() throws {
        var crypto = try session()
        _ = try crypto.open(data(status))
        XCTAssertThrowsError(try crypto.open(data(status)))
        var gap = data(status)
        gap[1] = 3
        XCTAssertThrowsError(try crypto.open(gap))
    }

    func testTamperedHeaderCiphertextAndTagAreRejected() throws {
        for offset in [0, 5, data(status).count - 1] {
            var crypto = try session()
            var corrupt = data(status)
            corrupt[offset] ^= 1
            XCTAssertThrowsError(try crypto.open(corrupt))
            // A rejected frame does not advance the receive counter.
            XCTAssertEqual(try crypto.open(data(status)), Data("s{\"usb\":\"ok\"}".utf8))
        }
    }

    func testWrongBoardKeyAndReplayedHandshakeFail() throws {
        XCTAssertThrowsError(try AppNetworkCrypto(master: Data(repeating: 0, count: 32),
                                                 clientNonce: clientNonce, welcome: data(welcome)))
        XCTAssertThrowsError(try AppNetworkCrypto(master: master, clientNonce: Data(33..<49), welcome: data(welcome)))
        XCTAssertThrowsError(try AppNetworkCrypto(master: master, clientNonce: clientNonce, welcome: Data()))
    }

    func testEndpointRejectsIncompleteOrInvalidKeys() {
        let id = UUID()
        XCTAssertNil(BoardNetworkEndpoint(ip: "192.168.2.3", key: "bad", name: "Board", pairedID: id).keyData)
        XCTAssertNil(BoardNetworkEndpoint(ip: "192.168.2.3", key: String(repeating: "z", count: 64), name: "Board", pairedID: id).keyData)
        XCTAssertEqual(BoardNetworkEndpoint(ip: "192.168.2.3", key: String(repeating: "00", count: 32), name: "Board", pairedID: id).keyData?.count, 32)
    }

    func testNetworkStatusAndProvisioningConfigDecode() throws {
        let config = try JSONDecoder().decode(NetConfig.self, from: Data("{\"mode\":\"sta\",\"ip\":\"192.168.2.3\",\"atok\":\"secret\",\"network_only\":true}".utf8))
        XCTAssertEqual(config.atok, "secret")
        XCTAssertEqual(config.network_only, true)
        XCTAssertEqual(BoardStatus.decode(Data("{\"usb\":\"ok\",\"net\":\"sta\",\"ap\":\"streaming\"}".utf8))?.usb, "ok")
    }

    @MainActor func testEncryptedStatusReconnectsAfterSocketLoss() async throws {
        let listening = expectation(description: "loopback board listening")
        let recovered = expectation(description: "status arrived after a new authenticated session")
        let parameters = NWParameters.tcp
        parameters.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        let listener = try NWListener(using: parameters)
        var sessions: [NetworkTestSession] = []
        listener.stateUpdateHandler = { state in if case .ready = state { listening.fulfill() } }
        listener.newConnectionHandler = { connection in
            Task { @MainActor in
                let session = NetworkTestSession(connection: connection, master: self.master, number: sessions.count + 1)
                sessions.append(session)
                session.start()
            }
        }
        listener.start(queue: .main)
        await fulfillment(of: [listening], timeout: 5)
        let network = AppNetwork(port: try XCTUnwrap(listener.port).rawValue)
        defer { network.stop(); sessions.forEach { $0.connection.cancel() }; listener.cancel() }
        var handshakes = 0
        network.onReady = { handshakes += 1 }
        network.onStatus = { data in
            if BoardStatus.decode(data)?.up == 2 { recovered.fulfill() }
        }
        network.start(BoardNetworkEndpoint(ip: "127.0.0.1", key: master.map { String(format: "%02x", $0) }.joined(),
                                           name: "Test board", pairedID: UUID()))
        await fulfillment(of: [recovered], timeout: 12)
        XCTAssertEqual(handshakes, 2)
        XCTAssertEqual(sessions.count, 2)
        XCTAssertTrue(network.ready)
        XCTAssertTrue(sessions.allSatisfy(\.verified))
    }
}

/// An independent board peer: splits the welcome record, coalesces reply/status records, then drops TCP.
@MainActor private final class NetworkTestSession {
    let connection: NWConnection
    let master: Data
    let number: Int
    private var incoming = Data()
    private var keys: AppNetworkCrypto?
    private var receiveCounter: UInt32 = 0
    private var sendCounter: UInt32 = 0
    private(set) var verified = false

    init(connection: NWConnection, master: Data, number: Int) {
        self.connection = connection; self.master = master; self.number = number
    }
    func start() { connection.start(queue: .main); receive() }
    private func receive() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 4096) { [weak self] data, _, done, error in
            Task { @MainActor in
                guard let self else { return }
                do {
                    if let data {
                        self.incoming.append(data)
                        while self.incoming.count >= 2 {
                            let count = Int(self.incoming[0]) | Int(self.incoming[1]) << 8
                            guard count > 0 && count <= 1100 else { throw DeviceError(message: "bad test record") }
                            if self.incoming.count < count + 2 { break }
                            let frame = Data(self.incoming.dropFirst(2).prefix(count))
                            self.incoming = Data(self.incoming.dropFirst(count + 2))
                            try self.handle(frame)
                        }
                    }
                    if !done && error == nil { self.receive() }
                } catch { XCTFail("test board: \(error)"); self.connection.cancel() }
            }
        }
    }
    private func record(_ data: Data) -> Data { Data([UInt8(data.count & 255), UInt8(data.count >> 8)]) + data }
    private func handle(_ frame: Data) throws {
        if keys == nil {
            XCTAssertEqual(frame.first, 0x02)
            let client = Data(frame.dropFirst())
            let board = Data(48..<64)
            let proof = Data(HMAC<SHA256>.authenticationCode(for: Data("app-welcome-v1".utf8) + client + board,
                                                            using: SymmetricKey(data: master))).prefix(16)
            let welcome = Data([0x82]) + board + proof
            keys = try AppNetworkCrypto(master: master, clientNonce: client, welcome: welcome)
            let bytes = record(welcome)
            connection.send(content: bytes.prefix(1), completion: .contentProcessed { _ in })
            connection.send(content: bytes.dropFirst(), completion: .contentProcessed { _ in })
            return
        }
        let key = try XCTUnwrap(keys).sending
        receiveCounter += 1
        let bytes = Array(frame)
        let counter = (0..<4).reduce(UInt32(0)) { $0 | UInt32(bytes[1+$1]) << (8*$1) }
        XCTAssertEqual(counter, receiveCounter)
        let box = try AES.GCM.SealedBox(nonce: iv(67, counter), ciphertext: Data(frame.dropFirst(5).dropLast(16)),
                                       tag: Data(frame.suffix(16)))
        let plain = try AES.GCM.open(box, using: key, authenticating: Data(frame.prefix(5)))
        XCTAssertEqual(String(decoding: plain, as: UTF8.self), "c0 ping")
        if !verified {
            verified = true
            let output = try seal("r0 ok") + seal("s{\"usb\":\"ok\",\"up\":\(number)}")
            connection.send(content: output, completion: .contentProcessed { _ in })
            if number == 1 {
                Task { @MainActor in
                    try? await Task.sleep(for: .milliseconds(300))
                    self.connection.cancel()
                }
            }
        }
    }
    private func iv(_ stream: UInt8, _ counter: UInt32) throws -> AES.GCM.Nonce {
        var bytes = [UInt8](repeating: 0, count: 12); bytes[0] = stream
        for i in 0..<4 { bytes[8+i] = UInt8(truncatingIfNeeded: counter >> (8*i)) }
        return try AES.GCM.Nonce(data: bytes)
    }
    private func seal(_ text: String) throws -> Data {
        sendCounter += 1
        var header = Data([0x20])
        for i in 0..<4 { header.append(UInt8(truncatingIfNeeded: sendCounter >> (8*i))) }
        let box = try AES.GCM.seal(Data(text.utf8), using: try XCTUnwrap(keys).receiving,
                                   nonce: iv(66, sendCounter), authenticating: header)
        return record(header + box.ciphertext + box.tag)
    }
}
