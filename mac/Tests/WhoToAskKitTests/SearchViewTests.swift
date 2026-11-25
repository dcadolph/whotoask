import AppKit
import SwiftUI
import Testing

@testable import WhoToAsk
@testable import WhoToAskKit

// Spoken records what the search window hands VoiceOver to read.
@MainActor
final class Spoken {
    // names are the names handed over, in order.
    var names: [String] = []
}

// SearchWindow is the real search window hosted over the demo index.
@MainActor
struct SearchWindow {
    // hosted is the search window in its fence.
    let hosted: Hosted<AnyView>
    // model answers the field.
    let model: SearchModel
    // core is the open demo index.
    let core: Core
    // opened records the links the window opens.
    let opened: Opened
    // spoken records the names handed to VoiceOver.
    let spoken: Spoken
    // dir is the demo index directory, removed by close.
    let dir: URL

    // open hosts the search window over a fresh demo index, answering after delay, under the
    // system contrast setting contrast.
    static func open(
        delay: Duration = .milliseconds(1), contrast: ColorSchemeContrast = .standard
    ) throws -> SearchWindow {
        let dir = try demoDir()
        let opened = Opened()
        let spoken = Spoken()
        let core = try Core.open(dir: dir, key: "", secrets: CoreMemorySecrets(MemorySecrets()))
        let model = SearchModel(core: core, delay: delay, opener: opened.open)
        let view = SearchView(model: model, speak: { spoken.names.append($0) })
            .environment(\._colorSchemeContrast, contrast)
        let hosted = host(AnyView(view), size: NSSize(width: 360, height: 500))
        return SearchWindow(
            hosted: hosted, model: model, core: core, opened: opened, spoken: spoken, dir: dir
        )
    }

    // ask types text into the field and waits until the window shows the answer to all of it
    // with count people, so an answer to a shorter question still on its way cannot land later.
    func ask(_ text: String, people count: Int) async -> Bool {
        hosted.type(text)
        return await waitFor { model.answered == text && model.people.count == count }
    }

    // rows returns each person row's frame, in view points from the top left, one per row of
    // people in order. AppKit backs each plain row button with a view of the row's exact size,
    // so the rows are the views that span the content's full width below the search field,
    // which is the topmost of them. A person page has none.
    func rows() -> [CGRect] {
        let view = hosted.view
        let width = view.bounds.width - 24
        let wide = view.subviews.map { place($0.frame) }
            .filter { $0.minX == 12 && $0.width == width }
            .sorted { $0.minY < $1.minY }
        return Array(wide.dropFirst())
    }

    // place turns a frame in the hosting view into view points from the top left.
    func place(_ frame: CGRect) -> CGRect {
        if hosted.view.isFlipped { return frame }
        let top = hosted.view.bounds.height - frame.maxY
        return CGRect(x: frame.minX, y: top, width: frame.width, height: frame.height)
    }

    // lines returns the height, in view points from the top, of each person's name line, one per
    // row of people in order. The name sits on the row's first line, under three points of
    // padding.
    func lines() -> [CGFloat] {
        rows().map { $0.minY + 11 }
    }

    // marked returns the rows, counted from zero, that carry the mark in the rendered window,
    // read on each row's name line far enough right that no name reaches it, where only the
    // mark's fill is drawn.
    func marked() throws -> [Int] {
        let rep = try hosted.render()
        return lines().enumerated().filter { ink(rep, x: 300, y: $0.element) > 0.03 }.map(\.offset)
    }

    // edge returns the strongest ink just left of row's name line, where only the mark's outline
    // is drawn, from zero for nothing to one for solid.
    func edge(_ row: Int) throws -> CGFloat {
        let rep = try hosted.render()
        let found = lines()
        try #require(found.count > row, "lines \(found)")
        return stride(from: 0, to: 11, by: 0.5).map { ink(rep, x: $0, y: found[row]) }.max() ?? 0
    }

    // close takes the window down and removes the demo index.
    func close() {
        hosted.window.close()
        try? FileManager.default.removeItem(at: dir)
    }
}

extension ViewTests {
    // SearchViewTests drive the real search window with key presses and read what it draws.
    @MainActor
    @Suite
    struct SearchViewTests {
        // Test 0: Down twice marks the third person, the window and its content keep their size
        // while the mark moves, so no row jumps, and Enter opens the third person's link.
        @Test func downTwiceThenEnter() async throws {
            let search = try SearchWindow.open()
            defer { search.close() }
            #expect(await search.ask("billing kafka", people: 4))
            let names = search.model.people.map(\.name)
            await search.hosted.steady()
            #expect(search.rows().count == 4)
            #expect(try search.marked() == [0])
            #expect(search.model.enterHint == "Enter messages the first person.")
            let frame = search.hosted.window.frame
            let fitting = search.hosted.view.fittingSize
            let rows = search.rows()
            search.hosted.press(.down)
            search.hosted.press(.down)
            #expect(search.model.selected == 2)
            #expect(search.model.enterHint == "Enter messages \(names[2]).")
            await search.hosted.steady()
            #expect(try search.marked() == [2])
            #expect(search.hosted.window.frame == frame)
            #expect(search.hosted.view.fittingSize == fitting)
            #expect(search.rows() == rows)
            search.hosted.press(.enter)
            #expect(await waitFor(3) { !search.opened.urls.isEmpty })
            let third = try await search.core.person(search.model.people[2].id)
            let want = try #require(third.links?.first)
            #expect(search.opened.urls.map(\.absoluteString) == [want])
            search.hosted.expectUntouched()
        }

        // Test 1: Escape on a person's page goes back to the list with the same person marked, and
        // Up stops at the first row.
        @Test func escapeKeepsTheMark() async throws {
            let search = try SearchWindow.open()
            defer { search.close() }
            #expect(await search.ask("billing kafka", people: 4))
            let names = search.model.people.map(\.name)
            search.hosted.press(.down)
            search.hosted.press(.down)
            search.hosted.press(.commandEnter)
            #expect(await waitFor(3) { search.model.detail?.name == names[2] })
            await search.hosted.steady()
            #expect(search.rows().isEmpty)
            search.hosted.press(.escape)
            #expect(await waitFor(3) { search.model.detail == nil })
            #expect(search.model.selected == 2)
            await search.hosted.steady()
            #expect(try search.marked() == [2])
            for _ in 0..<4 { search.hosted.press(.up) }
            #expect(search.model.selected == 0)
            await search.hosted.steady()
            #expect(try search.marked() == [0])
            #expect(search.opened.urls.isEmpty)
            search.hosted.expectUntouched()
        }

        // Test 2: Command-Return pressed right after typing a new question shows the first person
        // of the new answer, not the person marked in the old one.
        @Test func commandReturnRightAfterTyping() async throws {
            let search = try SearchWindow.open(delay: .milliseconds(200))
            defer { search.close() }
            #expect(await search.ask("billing", people: 3))
            search.hosted.press(.down)
            let old = search.model.marked?.name
            for _ in 0..<"billing".count { search.hosted.press(.delete) }
            search.hosted.type("terraform")
            search.hosted.press(.commandEnter)
            #expect(await waitFor(3) { search.model.detail != nil })
            #expect(search.model.detail?.name == "Carol Lee")
            #expect(old != "Carol Lee")
            settle(0.5)
            #expect(search.model.detail?.name == "Carol Lee")
            search.hosted.expectUntouched()
        }

        // Test 3: A last name alone that answers no one lists the closest people by name under
        // the field, and the window promises nothing for Enter.
        @Test func surnameListsClosestByName() async throws {
            let search = try SearchWindow.open()
            defer { search.close() }
            search.hosted.type("zzqx")
            let none = await waitFor {
                search.model.answered == "zzqx" && search.model.message != nil
            }
            #expect(none)
            await search.hosted.steady()
            let nobody = search.hosted.view.fittingSize.height
            for _ in 0..<4 { search.hosted.press(.delete) }
            search.hosted.type("vance")
            let listed = await waitFor {
                search.model.answered == "vance" && !search.model.closest.isEmpty
            }
            #expect(listed)
            #expect(search.model.people.isEmpty)
            #expect(search.model.closest.map(\.name).contains("Pam Vance"))
            #expect(search.model.message == nil)
            #expect(search.model.enterHint == nil)
            await search.hosted.steady()
            #expect(search.hosted.view.fittingSize.height > nobody + 20)
            search.hosted.expectUntouched()
        }

        // Test 4: Under Increase Contrast the marked person carries an outline and the others do
        // not, and at the standard setting no row does.
        @Test func increasedContrastOutlinesTheMark() async throws {
            let cases: [(ColorSchemeContrast, Bool)] = [(.increased, true), (.standard, false)]
            for (contrast, outlined) in cases {
                let search = try SearchWindow.open(contrast: contrast)
                defer { search.close() }
                #expect(await search.ask("billing kafka", people: 4))
                await search.hosted.steady()
                let marked = try search.edge(0)
                let plain = try search.edge(1)
                #expect((marked > 0.4) == outlined, "\(contrast) marked edge \(marked)")
                #expect(plain < 0.05, "\(contrast) unmarked edge \(plain)")
            }
        }

        // Test 5: Each arrow press that moves the mark hands VoiceOver the newly marked name, and a
        // press at either end hands it nothing.
        @Test func arrowsSpeakEachMove() async throws {
            let search = try SearchWindow.open()
            defer { search.close() }
            #expect(await search.ask("billing kafka", people: 4))
            let names = search.model.people.map(\.name)
            for _ in 0..<4 { search.hosted.press(.down) }
            #expect(search.spoken.names == [names[1], names[2], names[3]])
            for _ in 0..<4 { search.hosted.press(.up) }
            #expect(search.spoken.names == [
                names[1], names[2], names[3], names[2], names[1], names[0],
            ])
            search.hosted.expectUntouched()
        }
    }
}
