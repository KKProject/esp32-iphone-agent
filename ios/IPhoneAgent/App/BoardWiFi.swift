import SwiftUI

/// Putting the board on the local network: the app writes the Wi-Fi name and password to `net` over BLE, the board
/// joins and reports its address: idle → applying (≤ 30 s) → joined, or failed(reason). A new start cancels the old
/// task. The password stays in the form: it goes to the board, never to the log or the app's storage.
enum BoardWiFiState: Equatable {
    case idle, applying, joined
    case failed(String)

    var busy: Bool { self == .applying }
}

extension AppModel {
    /// The board has an address on the Wi-Fi. The iPhone has to be on the same network to mirror to it.
    var boardOnNetwork: Bool { board.connected && board.status?.net == "sta" }

    func applyBoardWiFi(ssid: String, password: String) {
        boardWiFiTask?.cancel()
        boardWiFiTask = Task { [weak self] in await self?.runBoardWiFi(ssid: ssid, password: password) }
    }

    private func runBoardWiFi(ssid: String, password: String) async {
        if let problem = Self.wifiProblem(ssid: ssid, password: password) {
            boardWiFi = .failed(problem)
            return
        }
        boardWiFi = .applying
        if MockMode.on {
            try? await Task.sleep(nanoseconds: 800_000_000)
            boardWiFi = .joined
            return mockStartMirroring()
        }
        do {
            try board.writeNet(NetConfig(mode: "sta", ssid: ssid, pass: password))
        } catch {
            say("wifi: could not reach the board: \(error.localizedDescription)")
            boardWiFi = .failed(String(localized: "Couldn't reach the board. Is it connected?"))
            return
        }
        say("wifi: asked the board to join \(ssid)")
        try? await Task.sleep(nanoseconds: 2_000_000_000)  // the board drops its old network first
        var why: Int?
        let end = now() + 30
        while now() < end {
            guard !Task.isCancelled else { return }
            if let net = try? await board.readNet(fresh: true), net.ssid == ssid {
                why = net.why
                if let ip = net.ip, !["", "0.0.0.0"].contains(ip) {
                    say("wifi: the board joined \(ssid) as \(ip)")
                    boardWiFi = .joined
                    return
                }
            }
            try? await Task.sleep(nanoseconds: 1_000_000_000)
        }
        say("wifi: the board didn't join \(ssid) (reason \(why ?? 0))")
        boardWiFi = .failed(Self.wifiFailure(why))
    }

    /// What's wrong with the form, before anything goes to the board.
    private static func wifiProblem(ssid: String, password: String) -> String? {
        if ssid.isEmpty || ssid.utf8.count > 32 { return String(localized: "Enter a Wi-Fi name of up to 32 bytes.") }
        if !password.isEmpty && !(8...64).contains(password.utf8.count) {
            return String(localized: "A Wi-Fi password has 8 to 64 characters. Leave it empty for an open network.")
        }
        return nil
    }

    /// The board's last disconnect reason (wifi_err_reason_t, `why` in `net`) in words.
    private static func wifiFailure(_ why: Int?) -> String {
        switch why {
        case 15, 202, 204: return String(localized: "The board couldn't sign in. Check the Wi-Fi password.")
        case 201, 210, 211, 212:
            return String(localized: "The board can't find this network. Check the name; the board only sees 2.4 GHz Wi-Fi.")
        default: return String(localized: "The board couldn't join this network. Check the name and password, then try again.")
        }
    }
}

/// The Wi-Fi name and password, for the setup page and Settings › Board.
struct BoardWiFiFields: View {
    @Binding var ssid: String
    @Binding var password: String
    @State private var reveal = false

    var body: some View {
        VStack(spacing: Space.s) {
            TextField("Wi-Fi Name", text: $ssid)
                .textContentType(.none)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            Divider()
            HStack {
                Group {
                    if reveal { TextField("Password", text: $password) } else { SecureField("Password", text: $password) }
                }
                .textContentType(.password)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                Button { reveal.toggle() } label: { Image(systemName: reveal ? "eye.slash" : "eye") }
                    .buttonStyle(.borderless)
                    .accessibilityLabel(Text(reveal ? "Hide Password" : "Show Password"))
            }
        }
    }
}

/// Settings › Board › Wi-Fi: where the board is now, and the form to move it to another network.
struct BoardWiFiSection: View {
    @ObservedObject var app: AppModel
    @ObservedObject var board: BoardLink
    @State private var ssid = ""
    @State private var password = ""
    @State private var address = ""
    @State private var connectionError: String?

    private var failure: String? { if case .failed(let why) = app.boardWiFi { return why } else { return nil } }

    var body: some View {
        Section {
            if let name = board.net?.ssid, !name.isEmpty { ValueRow(title: "Network", value: name) }
            if app.boardOnNetwork, let ip = board.status?.ip { ValueRow(title: "IP Address", value: ip) }
            if board.networkConfigured {
                ValueRow(title: "连接方式", value: board.usingNetwork ? "Wi-Fi" : "网络重连中")
                TextField("开发板 IP 地址", text: $address)
                    .keyboardType(.numbersAndPunctuation)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                Button("更新连接地址") {
                    do { try board.updateNetworkAddress(address); connectionError = nil }
                    catch { connectionError = error.localizedDescription }
                }
                Button("重新配置 Wi-Fi") {
                    Task {
                        do { try await board.restoreProvisioning(); connectionError = nil }
                        catch { connectionError = error.localizedDescription }
                    }
                }
                if let connectionError { Text(verbatim: connectionError).font(.footnote).foregroundStyle(.red) }
            } else {
                BoardWiFiFields(ssid: $ssid, password: $password)
                Button { app.applyBoardWiFi(ssid: ssid, password: password) } label: {
                    HStack {
                        Text(app.boardWiFi.busy ? "Connecting…" : "Connect the Board")
                        Spacer()
                        if app.boardWiFi.busy { ProgressView() }
                    }
                }
                .disabled(!board.connected || app.boardWiFi.busy || ssid.isEmpty)
                if let failure { Text(verbatim: failure).font(.footnote).foregroundStyle(.red) }
            }
        } header: {
            Text("Wi-Fi")
        } footer: {
            Text("首次配置通过蓝牙完成。配置后状态和操作均通过网络传输，开发板关闭蓝牙。请保持手机和开发板在同一网络。")
        }
        .onChange(of: board.networkAddress, initial: true) { _, ip in
            if let ip { address = ip }
        }
        .onChange(of: board.net?.ssid, initial: true) { _, name in
            if ssid.isEmpty, let name { ssid = name }
        }
    }
}
