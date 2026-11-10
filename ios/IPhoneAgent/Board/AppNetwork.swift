import CryptoKit
import Foundation
import Network
import Security

/// Persistent credentials arrive through bonded BLE. Commands/status use fresh, authenticated TCP sessions.
struct BoardNetworkEndpoint: Codable, Equatable {
    var ip: String
    var key: String
    var name: String
    var pairedID: UUID

    var keyData: Data? {
        guard key.count == 64 else { return nil }
        var bytes = [UInt8]()
        for offset in stride(from: 0, to: 64, by: 2) {
            let start = key.index(key.startIndex, offsetBy: offset)
            guard let byte = UInt8(key[start..<key.index(start, offsetBy: 2)], radix: 16) else { return nil }
            bytes.append(byte)
        }
        return Data(bytes)
    }
}

/// Full-length AES-GCM tags, with the firmware's existing HKDF derivation and directional nonces.
struct AppNetworkCrypto {
    let sending: SymmetricKey
    let receiving: SymmetricKey
    private(set) var sent: UInt32 = 0
    private(set) var received: UInt32 = 0

    init(master: Data, clientNonce: Data, welcome: Data) throws {
        guard master.count == 32, clientNonce.count == 16, welcome.count == 33, welcome.first == 0x82 else {
            throw DeviceError(message: "Invalid network handshake")
        }
        let boardNonce = Data(welcome.dropFirst().prefix(16))
        let proof = Data(HMAC<SHA256>.authenticationCode(for: Data("app-welcome-v1".utf8) + clientNonce + boardNonce,
                                                        using: SymmetricKey(data: master))).prefix(16)
        let different = zip(proof, welcome.suffix(16)).reduce(UInt8(0)) { $0 | ($1.0 ^ $1.1) }
        guard different == 0 else { throw DeviceError(message: "Board network identity did not verify") }
        let derived = HKDF<SHA256>.deriveKey(inputKeyMaterial: SymmetricKey(data: master),
                                            salt: clientNonce + boardNonce,
                                            info: Data("esp32agent-session-v1".utf8), outputByteCount: 32)
        let bytes = derived.withUnsafeBytes { Data($0) }
        sending = SymmetricKey(data: bytes.prefix(16))
        receiving = SymmetricKey(data: bytes.suffix(16))
    }

    private func nonce(_ stream: UInt8, _ counter: UInt32) throws -> AES.GCM.Nonce {
        var bytes = [UInt8](repeating: 0, count: 12)
        bytes[0] = stream
        for i in 0..<4 { bytes[8+i] = UInt8(truncatingIfNeeded: counter >> (8*i)) }
        return try AES.GCM.Nonce(data: bytes)
    }

    mutating func seal(_ plain: Data) throws -> Data {
        guard sent < .max else { throw DeviceError(message: "Network session expired") }
        sent += 1
        var header = Data([0x20])
        for i in 0..<4 { header.append(UInt8(truncatingIfNeeded: sent >> (8*i))) }
        let box = try AES.GCM.seal(plain, using: sending, nonce: nonce(67, sent), authenticating: header)
        return header + box.ciphertext + box.tag
    }

    mutating func open(_ frame: Data) throws -> Data {
        let bytes = Array(frame)
        guard bytes.count >= 22, bytes[0] == 0x20, received < .max else {
            throw DeviceError(message: "Invalid network frame")
        }
        let counter = (0..<4).reduce(UInt32(0)) { $0 | UInt32(bytes[1+$1]) << (8*$1) }
        guard counter == received + 1 else { throw DeviceError(message: "Network frame out of sequence") }
        let box = try AES.GCM.SealedBox(nonce: nonce(66, counter),
                                       ciphertext: Data(frame.dropFirst(5).dropLast(16)), tag: Data(frame.suffix(16)))
        let plain = try AES.GCM.open(box, using: receiving, authenticating: Data(frame.prefix(5)))
        received = counter
        return plain
    }
}

@MainActor final class AppNetwork {
    private let port: NWEndpoint.Port
    init(port: UInt16 = 7203) { self.port = NWEndpoint.Port(rawValue: port)! }
    var onReady: (() -> Void)?
    var onDown: ((String) -> Void)?
    var onReply: ((String) -> Void)?
    var onStatus: ((Data) -> Void)?
    private(set) var ready = false
    private var endpoint: BoardNetworkEndpoint?
    private var connection: NWConnection?
    private var crypto: AppNetworkCrypto?
    private var clientNonce = Data()
    private var incoming = Data()
    private var timer: Task<Void, Never>?
    private var lastReceive = 0.0, began = 0.0, lastPing = 0.0, retryAt = 0.0

    func start(_ endpoint: BoardNetworkEndpoint) {
        if self.endpoint == endpoint, timer != nil { return }
        stop()
        self.endpoint = endpoint
        connect()
        timer = Task { [weak self] in
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(1)) } catch { return }
                guard let self else { return }
                if self.connection == nil {
                    if now() >= self.retryAt { self.connect() }
                } else if now() - (self.ready ? self.lastReceive : self.began) > 6 {
                    self.fail("No fresh network status; reconnecting")
                } else if self.ready && now() - self.lastPing >= 2 {
                    do { try self.write("0 ping"); self.lastPing = now() } catch { self.fail(error.localizedDescription) }
                }
            }
        }
    }

    func kick() {
        guard endpoint != nil else { return }
        if connection != nil && now() - lastReceive > 6 { fail("Reconnecting after the App resumed") }
        if connection == nil { connect() }
    }

    func stop() {
        timer?.cancel(); timer = nil
        connection?.cancel(); connection = nil
        crypto = nil; incoming = Data(); ready = false; endpoint = nil
    }

    func reconnect() { fail("Reconnecting to the board over Wi-Fi") }

    private func connect() {
        guard let endpoint, endpoint.keyData != nil else { return }
        connection?.cancel()
        crypto = nil; incoming = Data(); ready = false
        began = now(); lastReceive = began
        let parameters = NWParameters.tcp
        parameters.prohibitedInterfaceTypes = [.cellular]
        let connection = NWConnection(host: NWEndpoint.Host(endpoint.ip), port: port, using: parameters)
        self.connection = connection
        connection.stateUpdateHandler = { [weak self, weak connection] state in
            Task { @MainActor in
                guard let self, let connection, self.connection === connection else { return }
                switch state {
                case .ready:
                    var bytes = [UInt8](repeating: 0, count: 16)
                    guard SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) == errSecSuccess else {
                        return self.fail("Could not create network session")
                    }
                    self.clientNonce = Data(bytes)
                    self.sendRecord(Data([0x02]) + self.clientNonce)
                    self.receive(connection)
                case .failed(let error), .waiting(let error): self.fail(error.localizedDescription)
                default: break
                }
            }
        }
        connection.start(queue: .main)
    }

    func write(_ line: String) throws {
        guard ready else { throw DeviceError(message: "Network board is not connected") }
        let plain = Data("c\(line)".utf8)
        guard plain.count + 21 <= 1100 else { throw DeviceError(message: "Network command is too long") }
        let frame = try crypto!.seal(plain)
        sendRecord(frame)
    }

    private func sendRecord(_ frame: Data) {
        guard let connection else { return }
        let header = Data([UInt8(truncatingIfNeeded: frame.count), UInt8(truncatingIfNeeded: frame.count >> 8)])
        connection.send(content: header + frame, completion: .contentProcessed { [weak self, weak connection] error in
            guard let error else { return }
            Task { @MainActor in
                guard let self, let connection, self.connection === connection else { return }
                self.fail(error.localizedDescription)
            }
        })
    }

    private func receive(_ connection: NWConnection) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 4096) { [weak self, weak connection] data, _, done, error in
            Task { @MainActor in
                guard let self, let connection, self.connection === connection else { return }
                do {
                    if let data { self.incoming.append(data); try self.consume() }
                    if let error { throw error }
                    if done { throw DeviceError(message: "Board network connection closed") }
                    self.receive(connection)
                } catch { self.fail(error.localizedDescription) }
            }
        }
    }

    private func consume() throws {
        while incoming.count >= 2 {
            let count = Int(incoming[incoming.startIndex]) | Int(incoming[incoming.startIndex + 1]) << 8
            guard (1...1100).contains(count) else { throw DeviceError(message: "Invalid network record size") }
            guard incoming.count >= count + 2 else { return }
            let frame = Data(incoming.dropFirst(2).prefix(count))
            incoming = Data(incoming.dropFirst(count + 2))
            if crypto == nil {
                guard let master = endpoint?.keyData else { throw DeviceError(message: "Missing board network key") }
                crypto = try AppNetworkCrypto(master: master, clientNonce: clientNonce, welcome: frame)
                ready = true
                try write("0 ping")  // proves possession of the session key before status is sent
                onReady?()
            } else {
                let plain = try crypto!.open(frame)
                switch plain.first {
                case 114: onReply?(String(decoding: plain.dropFirst(), as: UTF8.self))
                case 115: onStatus?(Data(plain.dropFirst()))
                default: throw DeviceError(message: "Unknown network message")
                }
            }
            lastReceive = now()
        }
    }

    private func fail(_ message: String) {
        connection?.cancel(); connection = nil
        crypto = nil; incoming = Data(); ready = false
        retryAt = now() + 2
        onDown?(message)
    }
}
