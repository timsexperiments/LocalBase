import XCTest

// Checks LocalBase's web UI in real iOS Safari on the Simulator, against a
// gateway on the Mac (LB_BASE_URL, default http://127.0.0.1:24795): no
// zoom when the composer gets focus, the composer above the software
// keyboard, Return adding a line on touch while Send submits, and Apple's
// accessibility audit (the engine behind Accessibility Inspector) on chat,
// the model picker, the history drawer, Admin and the Lab. Screenshots and
// the audit report are attachments; tools/ios-check.sh exports them.
final class LBSafariUITests: XCTestCase {
    let safari = XCUIApplication(bundleIdentifier: "com.apple.mobilesafari")
    var base: String { ProcessInfo.processInfo.environment["LB_BASE_URL"] ?? "http://127.0.0.1:24795" }

    override func setUp() {
        continueAfterFailure = true
    }

    private func open(_ path: String) {
        XCUIDevice.shared.system.open(URL(string: base + path)!)
        _ = safari.wait(for: .runningForeground, timeout: 20)
        _ = safari.webViews.firstMatch.waitForExistence(timeout: 20)
        sleep(2)
    }

    private func shot(_ name: String) {
        let a = XCTAttachment(screenshot: XCUIScreen.main.screenshot())
        a.name = name
        a.lifetime = .keepAlways
        add(a)
    }

    private func note(_ name: String, _ text: String) {
        let a = XCTAttachment(string: text)
        a.name = name
        a.lifetime = .keepAlways
        add(a)
    }

    private var composer: XCUIElement { safari.webViews.textViews.firstMatch }

    func test1ComposerFocusKeyboardAndReturn() throws {
        open("/app/chat")
        let field = composer
        XCTAssertTrue(field.waitForExistence(timeout: 15), "composer not found")
        shot("01-chat")
        let before = field.frame
        let webBefore = safari.webViews.firstMatch.frame
        field.tap()
        let keyboard = safari.keyboards.firstMatch
        XCTAssertTrue(keyboard.waitForExistence(timeout: 10), "no software keyboard after tapping the composer")
        sleep(1)
        let after = field.frame
        // A zoom on focus scales the page: the field's width changes.
        XCTAssertEqual(after.width, before.width, accuracy: 1.5, "the page zoomed when the composer got focus")
        note("focus-geometry", "composer before \(before), after \(after); web view \(webBefore)")
        // The composer stays above the keyboard.
        XCTAssertLessThanOrEqual(after.maxY, keyboard.frame.minY + 1, "the composer is under the keyboard")
        shot("02-keyboard-up")

        // Return adds a line on touch; it does not send.
        field.typeText("first line")
        field.typeText("\n")
        field.typeText("second line")
        let value = field.value as? String ?? ""
        note("textarea-value", value)
        XCTAssertTrue(value.contains("first line\nsecond line"), "Return did not add a line: \(value.debugDescription)")
        XCTAssertFalse(safari.webViews.staticTexts["hello from Qwen_Qwen3.5-9B-Q4_K_M.gguf"].exists, "Return sent the message")
        shot("03-two-lines")

        // Send submits.
        safari.webViews.buttons["Send"].tap()
        let reply = safari.webViews.staticTexts.containing(NSPredicate(format: "label CONTAINS 'hello from'")).firstMatch
        XCTAssertTrue(reply.waitForExistence(timeout: 30), "Send did not submit (no reply)")
        shot("04-sent")
    }

    private func audit(_ name: String) {
        var lines: [String] = []
        let web = safari.webViews.firstMatch.frame
        do {
            try safari.performAccessibilityAudit { issue in
                let el = issue.element
                let frame = el?.frame ?? .zero
                let inPage = web.contains(CGPoint(x: frame.midX, y: frame.midY))
                lines.append("\(inPage ? "PAGE" : "SAFARI") \(issue.auditType) | \(issue.compactDescription) | \(el?.elementType.rawValue ?? 0) '\(el?.label ?? "")' \(frame)" + (frame == .zero ? " desc=" + String(issue.detailedDescription.prefix(200)) + " el=" + String((el?.debugDescription ?? "nil").prefix(400)).replacingOccurrences(of: "\n", with: " / ") : ""))
                return true // record every issue; the report decides
            }
        } catch {
            lines.append("AUDIT ERROR \(error)")
        }
        note("audit-\(name)", lines.isEmpty ? "no issues" : lines.joined(separator: "\n"))
        shot("audit-\(name)")
    }

    func test2AccessibilityAudit() throws {
        open("/app/chat")
        audit("chat")
        let picker = safari.webViews.descendants(matching: .any).matching(NSPredicate(format: "label BEGINSWITH 'Choose model'")).firstMatch
        if picker.waitForExistence(timeout: 10) {
            picker.tap()
            sleep(2)
            audit("picker")
        } else {
            note("audit-picker", "AUDIT ERROR picker button not found")
        }
        open("/app/chat")
        let menu = safari.webViews.descendants(matching: .any).matching(NSPredicate(format: "label == 'Open menu and history'")).firstMatch
        if menu.waitForExistence(timeout: 10) {
            menu.tap()
            sleep(2)
            audit("history")
        } else {
            note("audit-history", "AUDIT ERROR menu button not found")
        }
        open("/app/admin")
        audit("admin")
        open("/app/lab?tab=image")
        audit("lab")
    }
}

final class LBTreeDump: XCTestCase {
    let safari = XCUIApplication(bundleIdentifier: "com.apple.mobilesafari")
    func testDumpTrees() throws {
        let base = ProcessInfo.processInfo.environment["LB_BASE_URL"] ?? "http://127.0.0.1:24795"
        for path in (ProcessInfo.processInfo.environment["LB_DUMP_PATHS"] ?? "/app/chat,/app/admin").split(separator: ",") {
            XCUIDevice.shared.system.open(URL(string: base + path)!)
            _ = safari.webViews.firstMatch.waitForExistence(timeout: 20)
            sleep(3)
            let a = XCTAttachment(string: safari.webViews.firstMatch.debugDescription)
            a.name = "tree-" + path.replacingOccurrences(of: "/", with: "_")
            a.lifetime = .keepAlways
            add(a)
            var lines: [String] = []
            try? safari.performAccessibilityAudit { issue in
                lines.append("\(issue.compactDescription) | \(issue.element?.elementType.rawValue ?? 0) '\(issue.element?.label ?? "")' \(issue.element?.frame ?? .zero)")
                return true
            }
            let b = XCTAttachment(string: lines.joined(separator: "\n"))
            b.name = "audit-" + path.replacingOccurrences(of: "/", with: "_")
            b.lifetime = .keepAlways
            add(b)
        }
    }
}
