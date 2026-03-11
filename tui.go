package main

import (
	"fmt"
	"strings"

	"github.com/rivo/tview"
)

type tuiState struct {
	ref            imageRef
	client         *registryClient
	platforms      []platformOption
	singleManifest *imageManifest
	selected       []bool
}

func runTUI(version string) error {
	app := tview.NewApplication()
	pages := tview.NewPages()
	state := &tuiState{}

	imageInput := tview.NewInputField().SetLabel("Image").SetText("alpine:latest")
	outputInput := tview.NewInputField().SetLabel("Output")
	proxyInput := tview.NewInputField().SetLabel("Proxy")
	userInput := tview.NewInputField().SetLabel("Username")
	passInput := tview.NewInputField().SetLabel("Password").SetMaskCharacter('*')
	insecure := false

	if defaultOut, err := defaultOutputTar(imageInput.GetText()); err == nil {
		outputInput.SetText(defaultOut)
	}

	imageInput.SetChangedFunc(func(text string) {
		if strings.TrimSpace(outputInput.GetText()) == "" {
			if out, err := defaultOutputTar(text); err == nil {
				outputInput.SetText(out)
			}
		}
	})

	form := tview.NewForm()
	form.AddFormItem(imageInput)
	form.AddFormItem(outputInput)
	form.AddFormItem(proxyInput)
	form.AddFormItem(userInput)
	form.AddFormItem(passInput)
	form.AddCheckbox("Insecure TLS", false, func(checked bool) {
		insecure = checked
	})
	form.AddButton("Fetch Architectures", func() {
		image := strings.TrimSpace(imageInput.GetText())
		if image == "" {
			showModal(app, pages, "Image is required")
			return
		}
		output := strings.TrimSpace(outputInput.GetText())
		if output == "" {
			defaultOut, err := defaultOutputTar(image)
			if err != nil {
				showModal(app, pages, err.Error())
				return
			}
			outputInput.SetText(defaultOut)
		}

		ref, err := parseImageRef(image)
		if err != nil {
			showModal(app, pages, err.Error())
			return
		}
		client, err := newRegistryClient(strings.TrimSpace(proxyInput.GetText()), strings.TrimSpace(userInput.GetText()), passInput.GetText(), insecure)
		if err != nil {
			showModal(app, pages, err.Error())
			return
		}
		platforms, singleManifest, err := resolvePlatforms(client, ref)
		if err != nil {
			showModal(app, pages, err.Error())
			return
		}
		state.ref = ref
		state.client = client
		state.platforms = platforms
		state.singleManifest = singleManifest
		state.selected = make([]bool, len(platforms))
		for i := range state.selected {
			state.selected[i] = true
		}
		showArchSelection(app, pages, state, outputInput)
	})
	form.AddButton("Quit", func() {
		app.Stop()
	})
	form.SetBorder(true).SetTitle(fmt.Sprintf("dia - Docker Image Archiver (%s)", version)).SetTitleAlign(tview.AlignLeft)

	instructions := tview.NewTextView().
		SetText("Direct Docker Registry API v2 saver. No Docker daemon required.\nUse checkboxes to choose architectures (defaults to all).")
	instructions.SetBorder(true).SetTitle("Instructions")

	layout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(instructions, 4, 1, false).
		AddItem(form, 0, 1, true)

	pages.AddPage("main", centered(90, 26, layout), true, true)

	return app.SetRoot(pages, true).Run()
}

func showArchSelection(app *tview.Application, pages *tview.Pages, state *tuiState, outputInput *tview.InputField) {
	form := tview.NewForm()
	for i, opt := range state.platforms {
		idx := i
		label := fmt.Sprintf("%d. %s", idx+1, opt.Platform.String())
		form.AddCheckbox(label, true, func(checked bool) {
			state.selected[idx] = checked
		})
	}
	form.AddButton("Save", func() {
		selected := make([]int, 0, len(state.selected))
		for i, ok := range state.selected {
			if ok {
				selected = append(selected, i)
			}
		}
		if len(selected) == 0 {
			showModal(app, pages, "Select at least one architecture")
			return
		}

		output := strings.TrimSpace(outputInput.GetText())
		if output == "" {
			showModal(app, pages, "Output file path is required")
			return
		}
		progress := tview.NewTextView().SetDynamicColors(true)
		progress.SetBorder(true).SetTitle("Saving")
		pages.AddAndSwitchToPage("progress", centered(100, 30, progress), true)
		writer := &tuiLogWriter{app: app, view: progress}

		go func() {
			err := writeDockerTar(state.client, state.ref, state.singleManifest, state.platforms, selected, output, writer)
			app.QueueUpdateDraw(func() {
				if err != nil {
					showModal(app, pages, "Save failed: "+err.Error())
					pages.SwitchToPage("arch")
					return
				}
				fmt.Fprintf(progress, "\nDone. Saved to %s\n", output)
			})
		}()
	})
	form.AddButton("Back", func() {
		pages.SwitchToPage("main")
	})
	form.AddButton("Quit", func() {
		app.Stop()
	})

	form.SetBorder(true).SetTitle("Architecture Selection (defaults to all)")
	pages.AddAndSwitchToPage("arch", centered(100, 30, form), true)
}

func showModal(app *tview.Application, pages *tview.Pages, message string) {
	modal := tview.NewModal().
		SetText(message).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			pages.RemovePage("modal")
		})
	pages.AddPage("modal", modal, true, true)
	app.SetFocus(modal)
}

func centered(width, height int, primitive tview.Primitive) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(primitive, height, 1, true).
			AddItem(nil, 0, 1, false), width, 1, true).
		AddItem(nil, 0, 1, false)
}

type tuiLogWriter struct {
	app  *tview.Application
	view *tview.TextView
}

func (w *tuiLogWriter) Write(p []byte) (int, error) {
	text := string(p)
	w.app.QueueUpdateDraw(func() {
		fmt.Fprint(w.view, text)
	})
	return len(p), nil
}
