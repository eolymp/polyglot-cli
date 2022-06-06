package main

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"github.com/antchfx/xmlquery"
	"github.com/eolymp/contracts/go/eolymp/atlas"
	"github.com/eolymp/contracts/go/eolymp/executor"
	"github.com/eolymp/contracts/go/eolymp/keeper"
	"github.com/eolymp/contracts/go/eolymp/typewriter"
	"github.com/eolymp/contracts/go/eolymp/wellknown"
	"github.com/eolymp/go-packages/httpx"
	"github.com/eolymp/go-packages/oauth"
	c "github.com/eolymp/polyglot/cmd/config"
	"github.com/mholt/archiver"
	"github.com/spf13/viper"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const DownloadsDir = "downloads"
const RepeatNumber = 10
const TimeSleep = time.Minute

var client httpx.Client
var atl *atlas.AtlasService
var tw *typewriter.TypewriterService
var conf c.Configuration

func main() {

	viper.SetConfigName("config")
	viper.AddConfigPath("./cmd/config")
	viper.AutomaticEnv()
	viper.SetConfigType("yml")

	if err := viper.ReadInConfig(); err != nil {
		log.Printf("Error reading config file, %s", err)
	}

	err := viper.Unmarshal(&conf)
	if err != nil {
		log.Printf("Unable to decode into struct, %v", err)
	}

	client = httpx.NewClient(
		&http.Client{Timeout: 300 * time.Second},
		httpx.WithCredentials(oauth.PasswordCredentials(
			oauth.NewClient(conf.Eolymp.ApiUrl),
			conf.Eolymp.Username,
			conf.Eolymp.Password,
		)),
		httpx.WithHeaders(map[string][]string{
			"Space-ID": {conf.SpaceId},
		}),
	)

	atl = atlas.NewAtlas(client)

	tw = typewriter.NewTypewriter(client)

	pid := flag.String("id", "", "Problem ID")
	flag.Parse()

	command := flag.Arg(0)

	if command == "ic" {
		contestId := flag.Arg(1)
		if contestId == "" {
			log.Println("Path argument is empty")
			flag.Usage()
			os.Exit(-1)
		}
		ImportContest(contestId)
	} else if command == "uc" {
		contestId := flag.Arg(1)
		if contestId == "" {
			log.Println("Path argument is empty")
			flag.Usage()
			os.Exit(-1)
		}
		UpdateContest(contestId)
	} else if command == "ip" {
		for i, path := 1, flag.Arg(1); path != ""; i, path = i+1, flag.Arg(i+1) {
			if err := ImportProblem(path, pid); err != nil {
				log.Fatal(err)
			}
		}
	} else if command == "dp" {
		for i, link := 1, flag.Arg(1); link != ""; i, link = i+1, flag.Arg(i+1) {
			if err := DownloadAndImportProblem(link, pid); err != nil {
				log.Fatal(err)
			}
		}
	} else {
		log.Println("Unknown command")
	}

}

func ImportContest(contestId string) {
	data := GetData()
	problems := GetProblems(contestId)
	log.Println(problems)
	var problemList []map[string]interface{}
	ctx := context.Background()
	for _, problem := range problems {
		pid, _ := CreateProblem(ctx)
		problemList = append(problemList, map[string]interface{}{"id": pid, "link": problem})
	}
	data[contestId] = problemList
	SaveData(data)
	log.Println(data)
	UpdateContest(contestId)
}

func UpdateContest(contestId string) {
	data := GetData()
	t := reflect.ValueOf(data[contestId])
	for i := 0; i < t.Len(); i++ {
		m := t.Index(i).Elem()
		g := make(map[string]string)
		iter := m.MapRange()
		for iter.Next() {
			g[iter.Key().String()] = iter.Value().Elem().String()
		}
		pid := g["id"]
		log.Println(pid, g["link"])

		if err := DownloadAndImportProblem(g["link"], &pid); err != nil {
			log.Println(err)
		}
	}
}

func DownloadAndImportProblem(link string, pid *string) error {
	path, err := DownloadProblem(link)
	if err != nil {
		log.Println("Failed to download problem")
		os.Exit(-1)
	}

	return ImportProblem(path, pid)
}

func DownloadProblem(link string) (path string, err error) {
	log.Println("Started polygon download")
	if conf.Polygon.Login == "" || conf.Polygon.Password == "" {
		return "", fmt.Errorf("no polygon credentials")
	}
	if _, err := os.Stat(DownloadsDir); os.IsNotExist(err) {
		err = os.Mkdir(DownloadsDir, 0777)
		if err != nil {
			log.Println("Failed to create dir")
			return "", err
		}
	}
	name := link[strings.LastIndex(link, "/")+1:]
	location := DownloadsDir + "/" + name
	if err := DownloadFileAndUnzip(link, conf.Polygon.Login, conf.Polygon.Password, location); err != nil {
		log.Println("Failed to download from polygon")
		return "", err
	}

	log.Println("Finished polygon download")
	return location, nil
}

func DownloadFileAndUnzip(URL, login, password, location string) error {
	response, err := http.PostForm(URL, url.Values{"login": {login}, "password": {password}, "type": {"windows"}})
	if err != nil {
		return err
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != 200 {
		return errors.New("non 200 status code")
	}

	file, err := os.Create(location + ".zip")
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()

	if _, err = io.Copy(file, response.Body); err != nil {
		return err
	}

	if _, err := os.Stat(location); !os.IsNotExist(err) {
		if err = os.RemoveAll(location); err != nil {
			return err
		}
	}

	if err = os.Mkdir(location, 0777); err != nil {
		return err
	}

	z := archiver.Zip{
		CompressionLevel:       flate.DefaultCompression,
		MkdirAll:               true,
		SelectiveCompression:   true,
		ContinueOnError:        true,
		OverwriteExisting:      true,
		ImplicitTopLevelFolder: false,
	}
	if err = z.Unarchive(location+".zip", location); err != nil {
		log.Println(err)
		return err
	}

	return nil
}

func CreateProblem(ctx context.Context) (string, error) {
	for i := 0; i < RepeatNumber; i++ {
		pout, err := atl.CreateProblem(ctx, &atlas.CreateProblemInput{Problem: &atlas.Problem{}})
		if err == nil {
			log.Printf("Problem created with ID %#v", pout.ProblemId)
			return pout.ProblemId, nil
		}
		log.Printf("Unable to create problem: %v", err)
		time.Sleep(TimeSleep)
	}
	return "", nil
}

func ImportProblem(path string, pid *string) error {

	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Printf("Import path %#v is invalid: %v", path, err)
		return err
	}

	spec := &Specification{}

	specf, err := os.Open(filepath.Join(path, "problem.xml"))
	if err != nil {
		log.Printf("Unable to open problem.xml: %v", err)
		return err
	}

	defer func() {
		_ = specf.Close()
	}()

	if err := xml.NewDecoder(specf).Decode(&spec); err != nil {
		log.Printf("Unable to parse problem.xml: %v", err)
		return err
	}

	if len(spec.Judging.Testsets) > 1 {
		log.Printf("More than 1 testset defined in problem.xml, only first one will be imported")
	}

	ctx := context.Background()

	statements := map[string]*atlas.Statement{}
	solutions := map[string]*atlas.Solution{}
	testsets := map[uint32]*atlas.Testset{}
	tests := map[string]*atlas.Test{}

	// create problem
	if *pid == "" {
		*pid, err = CreateProblem(ctx)
		if err != nil {
			log.Printf("Unable to create problem: %v", err)
			return err
		}
	} else {
		stout, err := atl.ListStatements(ctx, &atlas.ListStatementsInput{ProblemId: *pid})
		if err != nil {
			log.Printf("Unable to list problem statements in Atlas: %v", err)
			return err
		}

		log.Printf("Found %v existing statements", len(stout.GetItems()))

		for _, s := range stout.GetItems() {
			statements[s.GetLocale()] = s
		}

		eq := wellknown.ExpressionID{
			Is:    wellknown.ExpressionID_EQUAL,
			Value: *pid,
		}
		var filters []*wellknown.ExpressionID
		filters = append(filters, &eq)
		input := &atlas.ListSolutionsInput{Filters: &atlas.ListSolutionsInput_Filter{ProblemId: filters}}
		solout, err := atl.ListSolutions(ctx, input)
		if err != nil {
			log.Printf("Unable to list problem solutions in Atlas: %v", err)
			return err
		}

		log.Printf("Found %v existing solutions", len(solout.GetItems()))

		for _, s := range solout.GetItems() {
			solutions[s.GetLocale()] = s
		}

		tsout, err := atl.ListTestsets(ctx, &atlas.ListTestsetsInput{ProblemId: *pid})
		if err != nil {
			log.Printf("Unable to list problem testsets in Atlas: %v", err)
			return err
		}

		log.Printf("Found %v existing testsets", len(tsout.GetItems()))

		for _, ts := range tsout.GetItems() {
			testsets[ts.GetIndex()] = ts

			ttout, err := atl.ListTests(ctx, &atlas.ListTestsInput{TestsetId: ts.GetId()})
			if err != nil {
				log.Printf("Unable to list problem tests in Atlas: %v", err)
				return err
			}

			log.Printf("Found %v existing tests in testset %v", len(ttout.GetItems()), ts.Index)

			for _, tt := range ttout.GetItems() {
				tests[fmt.Sprint(ts.Index, "/", tt.Index)] = tt
			}
		}
	}

	templateLanguages := map[string][]string{
		"files/template_cpp.cpp":   {"gpp", "cpp:17-gnu10"},
		"files/template_java.java": {"java"},
		"files/template_pas.pas":   {"fpc"},
		"files/template_py.py":     {"pypy", "python"},
	}

	templates, err := atl.ListCodeTemplates(ctx, &atlas.ListCodeTemplatesInput{ProblemId: *pid})

	for _, template := range templates.GetItems() {
		_, _ = atl.DeleteCodeTemplate(ctx, &atlas.DeleteCodeTemplateInput{TemplateId: template.Id})
	}

	for _, file := range spec.Files {
		name := file.Source.Path
		if list, ok := templateLanguages[name]; ok {
			for _, lang := range list {
				template := &atlas.Template{}
				template.ProblemId = *pid
				template.Runtime = lang
				source, err := ioutil.ReadFile(filepath.Join(path, file.Source.Path))
				if err != nil {
					log.Printf("Unable to list problem tests in Atlas: %v", err)
					os.Exit(-1)
				}
				template.Source = string(source)
				_, _ = atl.CreateCodeTemplate(ctx, &atlas.CreateCodeTemplateInput{ProblemId: *pid, Template: template})
				log.Printf("Added a template for %s", lang)
			}
		}
	}

	// set verifier
	verifier, err := MakeVerifier(path, spec)
	if err != nil {
		log.Printf("Unable to create E-Olymp verifier from specification in problem.xml: %v", err)
		return err
	}

	if _, err = atl.UpdateVerifier(ctx, &atlas.UpdateVerifierInput{ProblemId: *pid, Verifier: verifier}); err != nil {
		log.Printf("Unable to update problem verifier: %v", err)
		return err
	}

	log.Printf("Updated verifier")

	// set interactor

	if len(spec.Interactor.Sources) != 0 {
		interactor, err := MakeInteractor(path, spec)
		if err != nil {
			log.Printf("Unable to create E-Olymp interactor from specification in problem.xml: %v", err)
			return err
		}

		if _, err = atl.UpdateInteractor(ctx, &atlas.UpdateInteractorInput{ProblemId: *pid, Interactor: interactor}); err != nil {
			log.Printf("Unable to update problem interactor: %v", err)
			return err
		}

		log.Printf("Updated interactor")
	} else {
		log.Printf("No interactor found")
	}

	// create testsets
	if len(spec.Judging.Testsets) > 0 {
		testset := spec.Judging.Testsets[0]

		// read tests by group
		groupTests := map[uint32][]SpecificationTest{}
		testIndex := map[string]int{}
		for gi, test := range testset.Tests {
			groupTests[test.Group] = append(groupTests[test.Group], test)
			testIndex[fmt.Sprint(test.Group, "/", len(groupTests[test.Group]))] = gi
		}

		groups := testset.Groups
		if len(groups) == 0 {
			groups = []SpecificationGroup{
				{FeedbackPolicy: "icpc-expanded", Name: 0, Points: 100, PointsPolicy: "each-test"},
			}
		}

		for _, group := range groups {
			xts, ok := testsets[group.Name]
			if !ok {
				xts = &atlas.Testset{}
			}

			delete(testsets, group.Name)

			xts.Index = group.Name
			xts.TimeLimit = uint32(testset.TimeLimit)
			xts.MemoryLimit = uint64(testset.MemoryLimit)
			xts.FileSizeLimit = 536870912

			xts.ScoringMode = atlas.ScoringMode_EACH
			if group.PointsPolicy == "complete-group" {
				xts.ScoringMode = atlas.ScoringMode_ALL
			}

			xts.FeedbackPolicy = atlas.FeedbackPolicy_COMPLETE
			if group.FeedbackPolicy == "icpc" || group.FeedbackPolicy == "points" {
				xts.FeedbackPolicy = atlas.FeedbackPolicy_ICPC
			} else if group.FeedbackPolicy == "icpc-expanded" {
				xts.FeedbackPolicy = atlas.FeedbackPolicy_ICPC_EXPANDED
			}

			xts.Dependencies = nil
			for _, d := range group.Dependencies {
				xts.Dependencies = append(xts.Dependencies, d.Group)
			}

			if xts.Id != "" {
				_, err = UpdateTestset(ctx, &atlas.UpdateTestsetInput{TestsetId: xts.Id, Testset: xts})
				if err != nil {
					log.Printf("Unable to create testset: %v", err)
					return err
				}

				log.Printf("Updated testset %v", xts.Id)
			} else {
				out, err := CreateTestset(ctx, &atlas.CreateTestsetInput{ProblemId: *pid, Testset: xts})
				if err != nil {
					log.Printf("Unable to create testset: %v", err)
					return err
				}

				xts.Id = out.Id

				log.Printf("Created testset %v", xts.Id)
			}

			// upload tests

				for ti, ts := range groupTests[group.Name] {
					xtt, ok := tests[fmt.Sprint(xts.Index, "/", int32(ti+1))]
					if !ok {
						xtt = &atlas.Test{}
					}

					delete(tests, fmt.Sprint(xts.Index, "/", int32(ti+1)))

					// index in the test list from specification
					gi := testIndex[fmt.Sprint(xts.Index, "/", int32(ti+1))]

					log.Printf("Processing %v test %v (Global Index: %v, ID: %#v) in testset %v (example: %v)", ts.Method, ti, gi, xtt.Id, xts.Index, ts.Sample)

					input, err := MakeObject(filepath.Join(path, fmt.Sprintf(testset.InputPathPattern, gi+1)))
					if err != nil {
						log.Printf("Unable to upload test input data to E-Olymp: %v", err)
						return err
					}

					answer, err := MakeObject(filepath.Join(path, fmt.Sprintf(testset.AnswerPathPattern, gi+1)))
					if err != nil {
						log.Printf("Unable to upload test answer data to E-Olymp: %v", err)
						return err
					}

					xtt.Index = int32(ti + 1)
					xtt.Example = ts.Sample
					xtt.Score = ts.Points
					xtt.InputObjectId = input
					xtt.AnswerObjectId = answer

					if xts.FeedbackPolicy == atlas.FeedbackPolicy_ICPC_EXPANDED {
						score := 100 / len(groupTests[group.Name])
						if len(groupTests[group.Name])-ti <= 100%len(groupTests[group.Name]) {
							score++
						}
						xtt.Score = float32(score)
					}

					if xtt.Id == "" {
						out, err := CreateTest(ctx, &atlas.CreateTestInput{TestsetId: xts.Id, Test: xtt})
						if err != nil {
							log.Printf("Unable to create test: %v", err)
							return err
						}

						xtt.Id = out.Id

						log.Printf("Created test %v", xtt.Id)
					} else {
						if _, err := UpdateTest(ctx, &atlas.UpdateTestInput{TestId: xtt.Id, Test: xtt}); err != nil {
							log.Printf("Unable to update test: %v", err)
							return err
						}

						log.Printf("Updated test %v", xtt.Id)
					}
				}

		}
	}

	// remove unused objects
	for _, test := range tests {
		log.Printf("Deleting unused test %v", test.Id)
		if _, err := DeleteTest(ctx, &atlas.DeleteTestInput{TestId: test.Id}); err != nil {
			log.Printf("Unable to delete test: %v", err)
			return err
		}
	}

	for _, testset := range testsets {
		log.Printf("Deleting unused testset %v", testset.Id)
		if _, err := atl.DeleteTestset(ctx, &atlas.DeleteTestsetInput{TestsetId: testset.Id}); err != nil {
			log.Printf("Unable to delete testset: %v", err)
			return err
		}
	}

	newStatements := map[string]*atlas.Statement{}

	// get all statements
	for _, ss := range spec.Statements {
		if ss.Type != "application/x-tex" {
			continue
		}

		log.Printf("Processing statement in %#v", ss.Language)

		statement, err := MakeStatement(path, &ss, ctx)
		if err != nil {
			log.Printf("Unable to create E-Olymp statement from specification in problem.xml: %v", err)
			return err
		}

		newStatements[statement.GetLocale()] = statement
	}

	var mainStatement *atlas.Statement = nil

	if ukr, ok := newStatements["uk"]; ok {
		mainStatement = ukr
	} else if eng, ok := newStatements["en"]; ok {
		mainStatement = eng
	} else {
		mainStatement, _ = newStatements["ru"]
	}

	for _, lang := range []string{"uk", "en", "ru"} {
		if _, ok := newStatements[lang]; !ok {
			statement := *mainStatement
			statement.Locale = lang
			newStatements[lang] = &statement
		}
	}

	for _, statement := range newStatements {

		log.Printf("Updating language %v", statement.Locale)

		xs, ok := statements[statement.GetLocale()]
		if !ok {
			xs = statement
		} else {
			xs.Locale = statement.Locale
			xs.Title = statement.Title
			xs.Content = statement.Content
			xs.Format = statement.Format
			xs.Author = statement.Author
			xs.Source = statement.Source
		}

		delete(statements, statement.GetLocale())

		if xs.Id == "" {
			out, err := CreateStatement(ctx, &atlas.CreateStatementInput{ProblemId: *pid, Statement: xs})
			if err != nil {
				log.Printf("Unable to create statement: %v", err)
				return err
			}

			xs.Id = out.Id

			log.Printf("Created statement %v", xs.Id)
		} else {
			_, err = UpdateStatement(ctx, &atlas.UpdateStatementInput{StatementId: xs.Id, Statement: xs})
			if err != nil {
				log.Printf("Unable to create statement: %v", err)
				return err
			}

			log.Printf("Updated statement %v", xs.Id)
		}
	}

	// remove unused objects
	for _, statement := range statements {
		log.Printf("Deleting unused statement %v", statement.Id)
		if _, err := atl.DeleteStatement(ctx, &atlas.DeleteStatementInput{StatementId: statement.Id}); err != nil {
			log.Printf("Unable to delete statement: %v", err)
			return err
		}
	}

	// get all solutions
	for _, ss := range spec.Solutions {
		if ss.Type != "application/x-tex" {
			continue
		}

		log.Printf("Processing solution in %#v", ss.Language)

		solution, err := MakeSolution(path, &ss)
		if err != nil {
			log.Printf("Unable to create E-Olymp solution from specification in problem.xml: %v", err)
			return err
		}

		xs, ok := solutions[solution.GetLocale()]
		if !ok {
			xs = solution
		} else {
			xs.Locale = solution.Locale
			xs.Content = solution.Content
			xs.Format = solution.Format
		}

		delete(solutions, solution.GetLocale())

		if xs.Id == "" {
			out, err := atl.CreateSolution(ctx, &atlas.CreateSolutionInput{ProblemId: *pid, Solution: xs})
			if err != nil {
				log.Printf("Unable to create solution: %v", err)
				return err
			}

			xs.Id = out.SolutionId

			log.Printf("Created solution %v", xs.Id)
		} else {
			_, err = atl.UpdateSolution(ctx, &atlas.UpdateSolutionInput{SolutionId: xs.Id, Solution: xs})
			if err != nil {
				log.Printf("Unable to create solution: %v", err)
				return err
			}

			log.Printf("Updated solution %v", xs.Id)
		}
	}

	// remove unused objects
	for _, solution := range solutions {
		log.Printf("Deleting unused solution %v", solution.Id)
		if _, err := atl.DeleteSolution(ctx, &atlas.DeleteSolutionInput{SolutionId: solution.Id}); err != nil {
			log.Printf("Unable to delete solution: %v", err)
			return err
		}
	}

	log.Printf("Finished")

	return nil
}

func CreateTestset(ctx context.Context, input *atlas.CreateTestsetInput) (*atlas.CreateTestsetOutput, error) {
	for i := 0; i < RepeatNumber; i++ {
		out, err := atl.CreateTestset(ctx, input)
		if err == nil {
			return out, nil
		}
		log.Printf("Error while creating testset: %v", err)
		time.Sleep(TimeSleep)
	}
	return atl.CreateTestset(ctx, input)
}

func UpdateTestset(ctx context.Context, input *atlas.UpdateTestsetInput) (*atlas.UpdateTestsetOutput, error) {
	for i := 0; i < RepeatNumber; i++ {
		out, err := atl.UpdateTestset(ctx, input)
		if err == nil {
			return out, nil
		}
		log.Printf("Error while updating testset: %v", err)
		time.Sleep(TimeSleep)
	}
	return atl.UpdateTestset(ctx, input)
}

func CreateTest(ctx context.Context, input *atlas.CreateTestInput) (*atlas.CreateTestOutput, error) {
	for i := 0; i < RepeatNumber; i++ {
		out, err := atl.CreateTest(ctx, input)
		if err == nil {
			return out, nil
		}
		log.Printf("Error while creating test: %v", err)
		time.Sleep(TimeSleep)
	}
	return atl.CreateTest(ctx, input)
}

func UpdateTest(ctx context.Context, input *atlas.UpdateTestInput) (*atlas.UpdateTestOutput, error) {
	for i := 0; i < RepeatNumber; i++ {
		out, err := atl.UpdateTest(ctx, input)
		if err == nil {
			return out, nil
		}
		log.Printf("Error while updating test: %v", err)
		time.Sleep(TimeSleep)
	}
	return atl.UpdateTest(ctx, input)
}

func DeleteTest(ctx context.Context, input *atlas.DeleteTestInput) (*atlas.DeleteTestOutput, error) {
	for i := 0; i < RepeatNumber; i++ {
		out, err := atl.DeleteTest(ctx, input)
		if err == nil {
			return out, nil
		}
		log.Printf("Error while deleting test: %v", err)
		time.Sleep(TimeSleep)
	}
	return atl.DeleteTest(ctx, input)
}

func CreateStatement(ctx context.Context, input *atlas.CreateStatementInput) (*atlas.CreateStatementOutput, error) {
	for i := 0; i < RepeatNumber; i++ {
		out, err := atl.CreateStatement(ctx, input)
		if err == nil {
			return out, nil
		}
		log.Printf("Error while creating statement: %v", err)
		time.Sleep(TimeSleep)
	}
	return atl.CreateStatement(ctx, input)
}

func UpdateStatement(ctx context.Context, input *atlas.UpdateStatementInput) (*atlas.UpdateStatementOutput, error) {
	for i := 0; i < RepeatNumber; i++ {
		out, err := atl.UpdateStatement(ctx, input)
		if err == nil {
			return out, nil
		}
		log.Printf("Error while updating statement: %v", err)
		time.Sleep(TimeSleep)
	}
	return atl.UpdateStatement(ctx, input)
}

func MakeObject(path string) (key string, err error) {
	kpr := keeper.NewKeeper(client)

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}

	var out *keeper.CreateObjectOutput
	for i := 0; i < RepeatNumber; i++ {
		out, err = kpr.CreateObject(context.Background(), &keeper.CreateObjectInput{Data: data})
		if err == nil {
			return out.Key, nil
		}

		log.Printf("Error while uploading file: %v", err)
		time.Sleep(TimeSleep)
	}

	return "", err
}

func MakeVerifier(path string, spec *Specification) (*executor.Verifier, error) {
	switch spec.Checker.Name {
	case "std::rcmp4.cpp", // Single or more double, max any error 1E-4
		"std::ncmp.cpp": // Single or more int64, ignores whitespaces
		return &executor.Verifier{Type: executor.Verifier_TOKENS, Precision: 4, CaseSensitive: true}, nil
	case "std::rcmp6.cpp": // Single or more double, max any error 1E-6
		return &executor.Verifier{Type: executor.Verifier_TOKENS, Precision: 6, CaseSensitive: true}, nil
	case "std::rcmp9.cpp": // Single or more double, max any error 1E-9
		return &executor.Verifier{Type: executor.Verifier_TOKENS, Precision: 9, CaseSensitive: true}, nil
	case "std::wcmp.cpp": // Sequence of tokens
		return &executor.Verifier{Type: executor.Verifier_TOKENS, Precision: 5, CaseSensitive: true}, nil
	case "std::nyesno.cpp", // Zero or more yes/no, case insensetive
		"std::yesno.cpp": // Single yes or no, case insensetive
		return &executor.Verifier{Type: executor.Verifier_TOKENS, Precision: 5, CaseSensitive: false}, nil
	case "std::fcmp.cpp", // Lines, doesn't ignore whitespaces
		"std::hcmp.cpp", // Single huge integer
		"std::lcmp.cpp": // Lines, ignores whitespaces
		return &executor.Verifier{Type: executor.Verifier_LINES}, nil
	default:
		mapping := map[string][]string{
			"gpp":    {"c.gcc", "cpp.g++", "cpp.g++11", "cpp.g++14", "cpp.g++17", "cpp.ms", "cpp.msys2-mingw64-9-g++17"},
			"csharp": {"csharp.mono"},
			"d":      {"d"},
			"go":     {"go"},
			"java":   {"java11", "java8"},
			"kotlin": {"kotlin"},
			"fpc":    {"pas.dpr", "pas.fpc"},
			"php":    {"php.5"},
			"python": {"python.2", "python.3"},
			"pypy":   {"python.pypy2", "python.pypy3"},
			"ruby":   {"ruby"},
			"rust":   {"rust"},
		}

		for lang, types := range mapping {
			source, ok := SourceByType(spec.Checker.Sources, types...)
			if !ok {
				continue
			}

			log.Printf("Unknown checker name %#v, using source code", spec.Checker.Name)

			data, err := ioutil.ReadFile(filepath.Join(path, source.Path))
			if err != nil {
				return nil, err
			}

			return &executor.Verifier{
				Type:   executor.Verifier_PROGRAM,
				Source: string(data), // todo: actually read file
				Lang:   lang,
			}, nil
		}
	}

	return nil, errors.New("checker configuration is not supported")
}

func MakeInteractor(path string, spec *Specification) (*executor.Interactor, error) {

	mapping := map[string][]string{
		"gpp":    {"c.gcc", "cpp.g++", "cpp.g++11", "cpp.g++14", "cpp.g++17", "cpp.ms"},
		"csharp": {"csharp.mono"},
		"d":      {"d"},
		"go":     {"go"},
		"java":   {"java11", "java8"},
		"kotlin": {"kotlin"},
		"fpc":    {"pas.dpr", "pas.fpc"},
		"php":    {"php.5"},
		"python": {"python.2", "python.3"},
		"pypy":   {"python.pypy2", "python.pypy3"},
		"ruby":   {"ruby"},
		"rust":   {"rust"},
	}

	for lang, types := range mapping {
		source, ok := SourceByType(spec.Interactor.Sources, types...)
		if !ok {
			continue
		}

		log.Printf("Unknown interactor name %#v, using source code", spec.Checker.Name)

		data, err := ioutil.ReadFile(filepath.Join(path, source.Path))
		if err != nil {
			return nil, err
		}

		return &executor.Interactor{
			Source: string(data), // todo: actually read file
			Lang:   lang,
		}, nil
	}

	return nil, errors.New("interactor configuration is not supported")
}

func MakeStatement(path string, statement *SpecificationStatement, ctx context.Context) (*atlas.Statement, error) {
	locale, err := MakeStatementLocale(statement.Language)
	if err != nil {
		return nil, err
	}

	propdata, err := ioutil.ReadFile(filepath.Join(path, filepath.Dir(statement.Path), "problem-properties.json"))
	if err != nil {
		return nil, err
	}

	props := PolygonProblemProperties{}

	if err := json.Unmarshal(propdata, &props); err != nil {
		return nil, fmt.Errorf("unable to unmrashal problem-properties.json: %w", err)
	}

	parts := []string{props.Legend}
	if props.Input != "" {
		parts = append(parts, fmt.Sprintf("\\InputFile\n\n%v", props.Input))
	}

	if props.Interaction != "" {
		parts = append(parts, fmt.Sprintf("\\Interaction\n\n%v", props.Interaction))
	}

	if props.Output != "" {
		parts = append(parts, fmt.Sprintf("\\OutputFile\n\n%v", props.Output))
	}

	if props.Notes != "" {
		parts = append(parts, fmt.Sprintf("\\Note\n\n%v", props.Notes))
	}

	if props.Scoring != "" {
		parts = append(parts, fmt.Sprintf("\\Scoring\n\n%v", props.Scoring))
	}

	content := strings.Join(parts, "\n\n")

	content, err = UpdateContentWithPictures(ctx, content, path+"/statements/"+statement.Language+"/")
	if err != nil {
		return nil, err
	}

	return &atlas.Statement{
		Locale:  locale,
		Title:   props.Name,
		Content: content,
		Format:  atlas.Statement_TEX,
		Author:  props.AuthorName,
		Source:  conf.Source,
	}, nil
}

func MakeStatementLocale(lang string) (string, error) {
	switch lang {
	case "ukrainian", "russian", "english", "hungarian":
		return lang[:2], nil
	default:
		return lang, fmt.Errorf("unknown language %#v", lang)
	}
}

func MakeSolution(path string, solution *SpecificationSolution) (*atlas.Solution, error) {
	locale, err := MakeSolutionLocale(solution.Language)
	if err != nil {
		return nil, err
	}

	propdata, err := ioutil.ReadFile(filepath.Join(path, filepath.Dir(solution.Path), "problem-properties.json"))
	if err != nil {
		return nil, err
	}

	props := PolygonProblemProperties{}

	if err := json.Unmarshal(propdata, &props); err != nil {
		return nil, fmt.Errorf("unable to unmrashal problem-properties.json: %w", err)
	}

	parts := []string{props.Solution}
	if props.Input != "" {
		parts = append(parts, fmt.Sprintf("\\InputFile\n\n%v", props.Input))
	}

	return &atlas.Solution{
		Locale:  locale,
		Content: props.Solution,
		Format:  atlas.Solution_TEX,
	}, nil
}

func MakeSolutionLocale(lang string) (string, error) {
	switch lang {
	case "ukrainian", "russian", "english":
		return lang[:2], nil
	default:
		return lang, fmt.Errorf("unknown language %#v", lang)
	}
}

func FindFilesWithExtension(path string, exts []string) []string {
	var files []string
	_ = filepath.Walk(path, func(path string, f os.FileInfo, _ error) error {
		for _, ext := range exts {
			r, err := regexp.Match(ext, []byte(f.Name()))
			if err == nil && r {
				files = append(files, f.Name())
			}
		}
		return nil
	})
	return files
}

func UpdateContentWithPictures(ctx context.Context, content, source string) (string, error) {
	exts := []string{".png", ".jpeg", ".jpg", ".eps"}
	files := FindFilesWithExtension(source, exts)
	for _, file := range files {
		if strings.Contains(content, file) {
			data, err := ioutil.ReadFile(source + file)
			if err != nil {
				log.Println("Failed to read file " + file)
				return "", err
			}
			var output *typewriter.UploadAssetOutput
			for i := 0; i < RepeatNumber; i++ {
				output, err = tw.UploadAsset(ctx, &typewriter.UploadAssetInput{Filename: file, Data: data})
				if err == nil {
					break
				}
				log.Println("Error while uploading asset")
			}
			if err != nil {
				log.Println("Error while uploading asset")
				return "", err
			}
			content = strings.ReplaceAll(content, file, output.Link)
		}
	}
	return content, nil
}

func SaveData(data map[string]interface{}) {
	json, _ := json.Marshal(data)
	ioutil.WriteFile("data.json", json, 0644)
}

func GetData() map[string]interface{} {
	jsonFile, _ := os.Open("data.json")
	defer jsonFile.Close()
	byteValue, _ := ioutil.ReadAll(jsonFile)
	var result map[string]interface{}
	json.Unmarshal(byteValue, &result)
	return result
}

func GetProblems(contestId string) []string {
	response, err := http.PostForm("https://polygon.codeforces.com/c/" + contestId + "/contest.xml", url.Values{"login": {conf.Polygon.Login}, "password": {conf.Polygon.Password}, "type": {"windows"}})
	if err != nil {
		return nil
	}
	defer func() {
		_ = response.Body.Close()
	}()
	buf := new(bytes.Buffer)
	buf.ReadFrom(response.Body)
	doc, err := xmlquery.Parse(buf)
	if err != nil {
		panic(err)
	}
	var result []string
	for _, n := range xmlquery.Find(doc, "//contest/problems/problem/@url") {
		result = append(result, n.InnerText())
	}
	return result
}
